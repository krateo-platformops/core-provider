package deployment_test

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/krateo-platformops/core-provider/internal/tools/deployment"
	"github.com/stretchr/testify/assert"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
)

func TestCleanFromRestartAnnotation(t *testing.T) {
	// Setup
	deploymentObj := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: v1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: map[string]string{
						"kubectl.kubernetes.io/restartedAt": time.Now().Format(time.RFC3339),
					},
				},
			},
		},
	}

	// Act
	deployment.CleanFromRestartAnnotation(deploymentObj)

	// Assert
	assert.NotContains(t, deploymentObj.Spec.Template.Annotations, "kubectl.kubernetes.io/restartedAt")
}
