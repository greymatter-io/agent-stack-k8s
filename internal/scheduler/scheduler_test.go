package scheduler

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/buildkite/agent-stack-k8s/v2/api"
	"github.com/buildkite/agent-stack-k8s/v2/internal/monitor"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"
	corev1 "k8s.io/api/core/v1"
)

//go:generate mockgen -destination=mock_handler_test.go -source=scheduler.go -package scheduler_test

func TestJobPluginConversion(t *testing.T) {
	t.Parallel()
	pluginConfig := KubernetesPlugin{
		PodSpec: &corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Image:   "alpine:latest",
					Command: []string{"hello world a=b=c"},
					EnvFrom: []corev1.EnvFromSource{
						{
							ConfigMapRef: &corev1.ConfigMapEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: "some-configmap"},
							},
						},
					},
				},
			},
		},
		GitEnvFrom: []corev1.EnvFromSource{
			{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: "git-secret"},
				},
			},
		},
	}
	pluginsJSON, err := json.Marshal([]map[string]interface{}{
		{
			"github.com/buildkite-plugins/kubernetes-buildkite-plugin": pluginConfig,
		},
		{
			"github.com/buildkite-plugins/some-other-buildkite-plugin": map[string]interface{}{"foo": "bar"},
		},
	})
	require.NoError(t, err)

	input := &monitor.Job{
		CommandJob: api.CommandJob{
			Uuid: "abc",
			Env:  []string{fmt.Sprintf("BUILDKITE_PLUGINS=%s", string(pluginsJSON))},
		},
		Tag: "queue=kubernetes",
	}
	wrapper := NewJobWrapper(zaptest.NewLogger(t), input, api.Config{AgentTokenSecret: "token-secret"})
	result, err := wrapper.ParsePlugins().Build()
	require.NoError(t, err)

	require.Len(t, result.Spec.Template.Spec.Containers, 3)

	commandContainer := findContainer(t, result.Spec.Template.Spec.Containers, "container-0")
	commandEnv := findEnv(t, commandContainer.Env, "BUILDKITE_COMMAND")
	require.Equal(t, pluginConfig.PodSpec.Containers[0].Command[0], commandEnv.Value)

	var envFromNames []string
	for _, envFrom := range commandContainer.EnvFrom {
		if envFrom.ConfigMapRef != nil {
			envFromNames = append(envFromNames, envFrom.ConfigMapRef.Name)
		}
		if envFrom.SecretRef != nil {
			envFromNames = append(envFromNames, envFrom.SecretRef.Name)
		}
	}
	require.ElementsMatch(t, envFromNames, []string{"some-configmap", "git-secret"})

	tokenEnv := findEnv(t, commandContainer.Env, "BUILDKITE_AGENT_TOKEN")
	require.Equal(t, "token-secret", tokenEnv.ValueFrom.SecretKeyRef.Name)

	tagLabel := result.Labels[api.TagLabel]
	require.Equal(t, api.TagToLabel(input.Tag), tagLabel)

	pluginsEnv := findEnv(t, commandContainer.Env, "BUILDKITE_PLUGINS")
	require.Equal(t, pluginsEnv.Value, `[{"github.com/buildkite-plugins/some-other-buildkite-plugin":{"foo":"bar"}}]`)
}

func TestTagEnv(t *testing.T) {
	t.Parallel()
	logger := zaptest.NewLogger(t)

	pluginConfig := KubernetesPlugin{
		PodSpec: &corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Image:   "alpine:latest",
					Command: []string{"hello world a=b=c"},
				},
			},
		},
	}
	pluginsJSON, err := json.Marshal([]map[string]any{
		{
			"github.com/buildkite-plugins/kubernetes-buildkite-plugin": pluginConfig,
		},
	})
	require.NoError(t, err)

	input := &monitor.Job{
		CommandJob: api.CommandJob{
			Uuid: "abc",
			Env:  []string{fmt.Sprintf("BUILDKITE_PLUGINS=%s", string(pluginsJSON))},
		},
		Tag: "queue=kubernetes",
	}
	wrapper := NewJobWrapper(logger, input, api.Config{AgentTokenSecret: "token-secret"})
	result, err := wrapper.ParsePlugins().Build()
	require.NoError(t, err)

	container := findContainer(t, result.Spec.Template.Spec.Containers, "agent")
	assertEnvFieldPath(t, container, "BUILDKITE_K8S_NODE", "spec.nodeName")
	assertEnvFieldPath(t, container, "BUILDKITE_K8S_NAMESPACE", "metadata.namespace")
	assertEnvFieldPath(t, container, "BUILDKITE_K8S_SERVICE_ACCOUNT", "spec.serviceAccountName")
}

func assertEnvFieldPath(t *testing.T, container corev1.Container, envVarName, fieldPath string) {
	env := findEnv(t, container.Env, envVarName)

	t.Helper()
	if assert.NotNil(t, env) {
		assert.Equal(t, env.Value, "")
		hasFieldRef := assert.NotNil(t, env.ValueFrom) && assert.NotNil(t, env.ValueFrom.FieldRef)
		if hasFieldRef {
			assert.Equal(t, env.ValueFrom.FieldRef.FieldPath, fieldPath)
		}
	}

}

func TestJobWithNoKubernetesPlugin(t *testing.T) {
	t.Parallel()
	input := &monitor.Job{
		CommandJob: api.CommandJob{
			Uuid:    "abc",
			Command: "echo hello world",
		},
	}
	wrapper := NewJobWrapper(zaptest.NewLogger(t), input, api.Config{})
	result, err := wrapper.ParsePlugins().Build()
	require.NoError(t, err)

	require.Len(t, result.Spec.Template.Spec.Containers, 3)

	commandContainer := findContainer(t, result.Spec.Template.Spec.Containers, "container-0")
	commandEnv := findEnv(t, commandContainer.Env, "BUILDKITE_COMMAND")
	require.Equal(t, input.Command, commandEnv.Value)
	pluginsEnv := findEnv(t, commandContainer.Env, "BUILDKITE_PLUGINS")
	require.Nil(t, pluginsEnv)
}

func TestFailureJobs(t *testing.T) {
	t.Parallel()
	pluginsJSON, err := json.Marshal([]map[string]interface{}{
		{
			"github.com/buildkite-plugins/kubernetes-buildkite-plugin": `"some-invalid-json"`,
		},
	})
	require.NoError(t, err)

	input := &monitor.Job{
		CommandJob: api.CommandJob{
			Uuid: "abc",
			Env:  []string{fmt.Sprintf("BUILDKITE_PLUGINS=%s", string(pluginsJSON))},
		},
		Tag: "queue=kubernetes",
	}
	wrapper := NewJobWrapper(zaptest.NewLogger(t), input, api.Config{})
	_, err = wrapper.ParsePlugins().Build()
	require.Error(t, err)

	result, err := wrapper.BuildFailureJob(err)
	require.NoError(t, err)

	commandContainer := findContainer(t, result.Spec.Template.Spec.Containers, "container-0")
	commandEnv := findEnv(t, commandContainer.Env, "BUILDKITE_COMMAND")
	require.Equal(t,
		`echo "failed parsing Kubernetes plugin: json: cannot unmarshal string into Go value of type scheduler.KubernetesPlugin" && exit 1`,
		commandEnv.Value,
	)
}

func findContainer(t *testing.T, containers []corev1.Container, name string) corev1.Container {
	t.Helper()

	for _, container := range containers {
		if container.Name == name {
			return container
		}
	}
	require.FailNow(t, "container not found")

	return corev1.Container{}
}

func findEnv(t *testing.T, envs []corev1.EnvVar, name string) *corev1.EnvVar {
	for _, env := range envs {
		if env.Name == name {
			return &env
		}
	}
	return nil
}
