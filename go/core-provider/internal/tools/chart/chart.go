package chart

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gobuffalo/flect"
	"github.com/krateo-platformops/core-provider/apis/compositiondefinitions/v1alpha1"
	"github.com/krateo-platformops/core-provider/internal/tools/chart/chartmeta"
	contexttools "github.com/krateo-platformops/core-provider/internal/tools/context"
	"github.com/krateo-platformops/core-provider/internal/tools/resolvers"
	"github.com/krateo-platformops/core-provider/internal/tools/retry"
	"github.com/krateo-platformops/core-provider/internal/tools/strutil"
	"github.com/krateo-platformops/core-provider/internal/tools/tgzfs"
	"github.com/krateo-platformops/plumbing/helm/getter"
	"github.com/krateo-platformops/provider-runtime/pkg/logging"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"
)

const (
	defaultGroup = "composition.krateo.io"

	chartRetryAttempts     = 5
	chartRetryInitialDelay = 250 * time.Millisecond
	chartRetryMaximumDelay = 2 * time.Second
)

var chartGetter = getter.Get
var chartRetryWait = retry.Wait

func ChartInfoFromSpec(ctx context.Context, dyn dynamic.Interface, nfo *v1alpha1.ChartInfo) (pkg fs.FS, rootDir string, err error) {
	log := contexttools.LoggerFromCtx(ctx, logging.NewNopLogger())

	if nfo == nil {
		return nil, "", fmt.Errorf("chart infos cannot be nil")
	}
	opts := []getter.Option{
		getter.WithRepo(nfo.Repo),
		getter.WithVersion(nfo.Version),
		getter.WithInsecureSkipVerifyTLS(nfo.InsecureSkipVerifyTLS),
	}
	if nfo.Credentials != nil {
		secret, err := resolvers.GetSecret(ctx, dyn, nfo.Credentials.PasswordRef)
		if err != nil {
			return nil, "", fmt.Errorf("failed to get secret: %w", err)
		}
		opts = append(opts, getter.WithCredentials(nfo.Credentials.Username, secret))
	}
	bData, err := chartBytesFromSpecWithRetry(ctx, nfo.Url, opts, log)
	if err != nil {
		return nil, "", err
	}

	return ChartInfoFromBytes(ctx, bData)
}

func chartBytesFromSpecWithRetry(ctx context.Context, uri string, opts []getter.Option, log logging.Logger) ([]byte, error) {
	bData, err := retry.Do[[]byte](ctx, retry.Config[[]byte]{
		Attempts:     chartRetryAttempts,
		InitialDelay: chartRetryInitialDelay,
		MaximumDelay: chartRetryMaximumDelay,
		Wait:         chartRetryWait,
		Retryable:    isRetryableChartError,
		OnRetry: func(attempt int, nextDelay time.Duration, err error) {
			log.Warn("Retrying chart fetch", "uri", uri, "attempt", attempt, "next_delay", nextDelay, "error", err)
		},
	}, func(context.Context) ([]byte, error) {
		dat, _, err := chartGetter(ctx, uri, opts...)
		if err != nil {
			return nil, fmt.Errorf("failed to get chart: %w", err)
		}
		if dat == nil {
			return nil, fmt.Errorf("failed to get chart: empty response reader")
		}

		bData, err := io.ReadAll(dat)
		if err != nil {
			return nil, fmt.Errorf("failed to read chart: %w", err)
		}

		return bData, nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get chart after %d attempts: %w", chartRetryAttempts, err)
	}

	return bData, nil
}

func isRetryableChartError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.Canceled) {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	if isNonRetryableChartError(err) {
		return false
	}

	var netErr net.Error
	if errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary()) {
		return true
	}

	return true
}

func isNonRetryableChartError(err error) bool {
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsNotFound(err) || apierrors.IsInvalid(err) || apierrors.IsBadRequest(err) {
		return true
	}
	if statusErr, ok := err.(*apierrors.StatusError); ok {
		switch statusErr.ErrStatus.Code {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity, http.StatusBadRequest:
			return true
		}
	}

	return false
}

func ChartInfoFromBytes(ctx context.Context, bin []byte) (pkg fs.FS, rootDir string, err error) {
	pkg, err = tgzfs.New(bytes.NewBuffer(bin))
	if err != nil {
		return nil, "", err
	}

	all, err := fs.ReadDir(pkg, ".")
	if err != nil {
		return nil, "", err
	}

	if len(all) != 1 {
		return nil, "", fmt.Errorf("tgz archive should contain only one root dir")
	}

	rootDir = all[0].Name()

	return pkg, rootDir, nil
}

func ChartGroupVersionKind(tgzFS fs.FS, rootDir string) (schema.GroupVersionKind, error) {
	fin, err := tgzFS.Open(rootDir + "/Chart.yaml")
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	defer fin.Close()

	din, err := io.ReadAll(fin)
	if err != nil {
		return schema.GroupVersionKind{}, err
	}

	res := map[string]any{}
	if err := yaml.Unmarshal(din, &res); err != nil {
		return schema.GroupVersionKind{}, err
	}

	// Via chartmeta rather than res["version"].(string): the assertion PANICS, and sigs.k8s.io/yaml
	// resolves YAML 1.1 just as yaml.v2 does (measured), so `name: y` / `no` / `on` / `off` arrive as
	// bools and `version: 1.0` as a float64. All are valid Helm chart names, and a panic in a
	// reconcile loop is a crash loop rather than a failed reconcile.
	version, err := chartmeta.StringField(res, "version", rootDir+"/Chart.yaml")
	if err != nil {
		return schema.GroupVersionKind{}, err
	}
	name, err := chartmeta.StringField(res, "name", rootDir+"/Chart.yaml")
	if err != nil {
		return schema.GroupVersionKind{}, err
	}

	return schema.GroupVersionKind{
		Group:   defaultGroup,
		Version: fmt.Sprintf("v%s", strings.ReplaceAll(version, ".", "-")),
		Kind:    flect.Pascalize(strutil.ToGolangName(name)),
	}, nil
}

func ChartJsonSchema(tgzFS fs.FS, rootDir string) ([]byte, error) {
	fin, err := tgzFS.Open(rootDir + "/values.schema.json")
	if err != nil {
		return nil, err
	}
	defer fin.Close()

	return io.ReadAll(fin)
}
