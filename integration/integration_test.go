package integration

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"testing"
	"text/template"
	"time"

	"github.com/Khan/genqlient/graphql"
	"github.com/buildkite/agent-stack-k8s/api"
	"github.com/buildkite/agent-stack-k8s/cmd/controller"
	"github.com/buildkite/go-buildkite/v3/buildkite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	restconfig "sigs.k8s.io/controller-runtime/pkg/client/config"
)

const (
	repoHTTP = "https://github.com/buildkite/agent-stack-k8s"
	repoSSH  = "git@github.com:buildkite/agent-stack-k8s"
	branch   = "v2"
)

var (
	preservePipelines       bool
	deleteOrphanedPipelines bool
	cfg                     api.Config

	//go:embed fixtures/*
	fixtures embed.FS
)

// hacks to make --config work
func TestMain(m *testing.M) {
	if err := os.Chdir(".."); err != nil {
		log.Fatal(err)
	}
	cmd := controller.New()
	cmd.Flags().BoolVar(&preservePipelines, "preserve-pipelines", false, "preserve pipelines created by tests")
	cmd.Flags().BoolVar(&deleteOrphanedPipelines, "delete-orphaned-pipelines", false, "delete all pipelines matching agent-k8s-*")
	var err error
	cfg, err = controller.ParseConfig(cmd, os.Args[1:])
	if err != nil {
		log.Fatal(err)
	}
	if err := os.Chdir("integration"); err != nil {
		log.Fatal(err)
	}
	for i, v := range os.Args {
		if strings.Contains(v, "test") {
			os.Args[i] = v
		} else {
			os.Args[i] = ""
		}
	}
	os.Exit(m.Run())
}

func TestWalkingSkeleton(t *testing.T) {
	tc := testcase{
		T:       t,
		Fixture: "helloworld.yaml",
		Repo:    repoHTTP,
		GraphQL: api.NewClient(cfg.BuildkiteToken),
	}.Init()
	ctx := context.Background()
	pipelineID := tc.CreatePipeline(ctx)
	tc.StartController(ctx, cfg)
	build := tc.TriggerBuild(ctx, pipelineID)
	tc.AssertSuccess(ctx, build)
}

func TestSSHRepoClone(t *testing.T) {
	tc := testcase{
		T:       t,
		Fixture: "secretref.yaml",
		Repo:    repoSSH,
		GraphQL: api.NewClient(cfg.BuildkiteToken),
	}.Init()

	ctx := context.Background()
	_, err := tc.Kubernetes.CoreV1().Secrets(cfg.Namespace).Get(ctx, "agent-stack-k8s", v1.GetOptions{})
	require.NoError(t, err, "agent-stack-k8s secret must exist")

	pipelineID := tc.CreatePipeline(ctx)
	tc.StartController(ctx, cfg)
	build := tc.TriggerBuild(ctx, pipelineID)
	tc.AssertSuccess(ctx, build)
}

func TestPluginCloneFailsTests(t *testing.T) {
	tc := testcase{
		T:       t,
		Fixture: "unknown-plugin.yaml",
		Repo:    repoHTTP,
		GraphQL: api.NewClient(cfg.BuildkiteToken),
	}.Init()

	ctx := context.Background()

	pipelineID := tc.CreatePipeline(ctx)
	tc.StartController(ctx, cfg)
	build := tc.TriggerBuild(ctx, pipelineID)
	tc.AssertFail(ctx, build)
}

func TestMaxInFlight(t *testing.T) {
	tc := testcase{
		T:       t,
		Fixture: "parallel.yaml",
		Repo:    repoHTTP,
		GraphQL: api.NewClient(cfg.BuildkiteToken),
	}.Init()

	ctx := context.Background()

	pipelineID := tc.CreatePipeline(ctx)
	cfg := cfg
	cfg.MaxInFlight = 1
	tc.StartController(ctx, cfg)
	buildID := tc.TriggerBuild(ctx, pipelineID).Number

	for {
		build, _, err := tc.Buildkite.Builds.Get(cfg.Org, tc.PipelineName, fmt.Sprintf("%d", buildID), nil)
		require.NoError(t, err)
		if *build.State == "running" {
			require.LessOrEqual(t, *build.Pipeline.RunningJobsCount, 1)
		} else if *build.State == "passed" {
			break
		} else if *build.State == "scheduled" {
			time.Sleep(time.Second)
			continue
		} else {
			t.Fatalf("unexpected build state: %v", *build.State)
		}
	}
}

func TestCleanupOrphanedPipelines(t *testing.T) {
	if !deleteOrphanedPipelines {
		t.Skip("not cleaning orphaned pipelines")
	}
	ctx := context.Background()
	graphqlClient := api.NewClient(cfg.BuildkiteToken)

	pipelines, err := api.SearchPipelines(ctx, graphqlClient, cfg.Org, "agent-k8s-", 100)
	require.NoError(t, err)
	for _, pipeline := range pipelines.Organization.Pipelines.Edges {
		builds, err := api.GetBuilds(ctx, graphqlClient, fmt.Sprintf("%s/%s", cfg.Org, pipeline.Node.Name), []api.BuildStates{api.BuildStatesRunning}, 100)
		require.NoError(t, err)
		for _, build := range builds.Pipeline.Builds.Edges {
			_, err = api.BuildCancel(ctx, graphqlClient, api.BuildCancelInput{Id: build.Node.Id})
			require.NoError(t, err)
		}
		_, err = api.PipelineDelete(ctx, graphqlClient, api.PipelineDeleteInput{
			Id: pipeline.Node.Id,
		})
		require.NoError(t, err)
		t.Logf("deleted orphaned pipeline! %v", pipeline.Node.Name)
	}
}

type testcase struct {
	*testing.T
	Logger       *zap.Logger
	Fixture      string
	Repo         string
	GraphQL      graphql.Client
	Kubernetes   kubernetes.Interface
	Buildkite    *buildkite.Client
	PipelineName string // autogenerated
}

func (t testcase) Init() testcase {
	t.Helper()
	t.Parallel()

	t.PipelineName = fmt.Sprintf("agent-k8s-%s-%d", strings.ToLower(t.Name()), time.Now().UnixNano())
	t.Logger = zaptest.NewLogger(t)

	clientConfig, err := restconfig.GetConfig()
	require.NoError(t, err)
	clientset, err := kubernetes.NewForConfig(clientConfig)
	require.NoError(t, err)
	t.Kubernetes = clientset
	config, err := buildkite.NewTokenConfig(cfg.BuildkiteToken, false)
	require.NoError(t, err)

	t.Buildkite = buildkite.NewClient(config.Client())

	return t
}

func (t testcase) CreatePipeline(ctx context.Context) string {
	t.Helper()

	tpl, err := template.ParseFS(fixtures, fmt.Sprintf("fixtures/%s", t.Fixture))
	require.NoError(t, err)

	var steps bytes.Buffer
	require.NoError(t, tpl.Execute(&steps, map[string]string{
		"queue": t.PipelineName,
	}))
	pipeline, _, err := t.Buildkite.Pipelines.Create(cfg.Org, &buildkite.CreatePipeline{
		Name:       t.PipelineName,
		Repository: t.Repo,
		ProviderSettings: &buildkite.GitHubSettings{
			TriggerMode: strPtr("none"),
		},
		Configuration: steps.String(),
	})
	require.NoError(t, err)

	if !preservePipelines {
		EnsureCleanup(t.T, func() {
			_, err = t.Buildkite.Pipelines.Delete(cfg.Org, t.PipelineName)
			assert.NoError(t, err)
			t.Logf("deleted pipeline! %v", pipeline.Name)
		})
	}

	return *pipeline.GraphQLID
}

func (t testcase) StartController(ctx context.Context, cfg api.Config) {
	t.Helper()

	//start controller
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, nil)
	clientConfig, err := kubeConfig.ClientConfig()
	require.NoError(t, err)

	k8sClient, err := kubernetes.NewForConfig(clientConfig)
	require.NoError(t, err)

	runCtx, cancel := context.WithCancel(ctx)
	EnsureCleanup(t.T, cancel)

	cfg.Tags = []string{fmt.Sprintf("queue=%s", t.PipelineName)}
	go controller.Run(runCtx, k8sClient, cfg)
}

func (t testcase) TriggerBuild(ctx context.Context, pipelineID string) api.Build {
	t.Helper()

	// trigger build
	createBuild, err := api.BuildCreate(ctx, t.GraphQL, api.BuildCreateInput{
		PipelineID: pipelineID,
		Commit:     "HEAD",
		Branch:     branch,
	})
	require.NoError(t, err)
	EnsureCleanup(t.T, func() {
		if _, err := api.BuildCancel(ctx, t.GraphQL, api.BuildCancelInput{
			Id: createBuild.BuildCreate.Build.Id,
		}); err != nil {
			if !strings.Contains(err.Error(), "Build can't be canceled because it's already finished") {
				t.Logf("failed to cancel build: %v", err)
			}
		}
	})
	build := createBuild.BuildCreate.Build
	require.GreaterOrEqual(t, len(build.Jobs.Edges), 1)
	node := build.Jobs.Edges[0].Node
	_, ok := node.(*api.JobJobTypeCommand)
	require.True(t, ok)

	return build.Build
}

func (t testcase) AssertSuccess(ctx context.Context, build api.Build) {
	t.Helper()
	require.Equal(t, api.BuildStatesPassed, t.waitForBuild(ctx, build))

	config, err := buildkite.NewTokenConfig(cfg.BuildkiteToken, false)
	require.NoError(t, err)

	client := buildkite.NewClient(config.Client())
	job := build.Jobs.Edges[0].Node.(*api.JobJobTypeCommand)
	logs, _, err := client.Jobs.GetJobLog(cfg.Org, t.PipelineName, strconv.Itoa(build.Number), job.Uuid)
	require.NoError(t, err)
	require.NotNil(t, logs.Content)
	require.Contains(t, *logs.Content, "Buildkite Agent Stack for Kubernetes")

	artifacts, _, err := client.Artifacts.ListByBuild(cfg.Org, t.PipelineName, strconv.Itoa(build.Number), nil)
	require.NoError(t, err)
	require.Len(t, artifacts, 2)
	filenames := []string{*artifacts[0].Filename, *artifacts[1].Filename}
	require.Contains(t, filenames, "README.md")
	require.Contains(t, filenames, "CODE_OF_CONDUCT.md")
}

func (t testcase) AssertFail(ctx context.Context, build api.Build) {
	t.Helper()

	require.Equal(t, api.BuildStatesFailed, t.waitForBuild(ctx, build))
}

func (t testcase) waitForBuild(ctx context.Context, build api.Build) api.BuildStates {
	t.Helper()

	for {
		getBuild, err := api.GetBuild(ctx, t.GraphQL, build.Uuid)
		require.NoError(t, err)
		switch getBuild.Build.State {
		case api.BuildStatesPassed, api.BuildStatesFailed:
			return getBuild.Build.State
		default:
			t.Logger.Debug("sleeping", zap.Any("build state", getBuild.Build.State))
			time.Sleep(time.Second)
		}
	}
}

func strPtr(p string) *string {
	return &p
}
