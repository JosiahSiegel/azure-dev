package containerapps

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"slices"
	"strconv"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore/arm"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/runtime"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/streaming"
	"github.com/Azure/azure-sdk-for-go/sdk/resourcemanager/appcontainers/armappcontainers/v3"
	"github.com/azure/azure-dev/cli/azd/internal"
	azdinternal "github.com/azure/azure-dev/cli/azd/internal"
	"github.com/azure/azure-dev/cli/azd/pkg/account"
	"github.com/azure/azure-dev/cli/azd/pkg/cloud"
	"github.com/azure/azure-dev/cli/azd/pkg/convert"
	"github.com/azure/azure-dev/cli/azd/pkg/httputil"
	"github.com/azure/azure-dev/cli/azd/pkg/output"
	"github.com/benbjohnson/clock"
	"gopkg.in/yaml.v3"
)

// ContainerAppService exposes operations for managing Azure Container Apps
type ContainerAppService interface {
	// Gets the ingress configuration for the specified container app
	GetIngressConfiguration(
		ctx context.Context,
		subscriptionId,
		resourceGroup,
		appName string,
	) (*ContainerAppIngressConfiguration, error)
	DeployYaml(
		ctx context.Context,
		subscriptionId string,
		resourceGroupName string,
		appName string,
		containerAppYaml []byte,
		progressLog func(string),
	) error
	// Adds and activates a new revision to the specified container app
	AddRevision(
		ctx context.Context,
		subscriptionId string,
		resourceGroupName string,
		appName string,
		imageName string,
		progressLog func(string),
	) error
	ListSecrets(ctx context.Context,
		subscriptionId string,
		resourceGroupName string,
		appName string,
	) ([]*armappcontainers.ContainerAppSecret, error)
}

// NewContainerAppService creates a new ContainerAppService
func NewContainerAppService(
	credentialProvider account.SubscriptionCredentialProvider,
	httpClient httputil.HttpClient,
	clock clock.Clock,
	armClientOptions *arm.ClientOptions,
	portalUrlBase cloud.PortalUrlBase,
) ContainerAppService {
	return &containerAppService{
		credentialProvider: credentialProvider,
		httpClient:         httpClient,
		userAgent:          azdinternal.UserAgent(),
		clock:              clock,
		armClientOptions:   armClientOptions,
		portalUrlBase:      portalUrlBase,
	}
}

type containerAppService struct {
	credentialProvider account.SubscriptionCredentialProvider
	httpClient         httputil.HttpClient
	userAgent          string
	clock              clock.Clock
	armClientOptions   *arm.ClientOptions
	portalUrlBase      cloud.PortalUrlBase
}

type ContainerAppIngressConfiguration struct {
	HostNames []string
}

// Gets the ingress configuration for the specified container app
func (cas *containerAppService) GetIngressConfiguration(
	ctx context.Context,
	subscriptionId string,
	resourceGroup string,
	appName string,
) (*ContainerAppIngressConfiguration, error) {
	containerApp, err := cas.getContainerApp(ctx, subscriptionId, resourceGroup, appName)
	if err != nil {
		return nil, fmt.Errorf("failed retrieving container app properties: %w", err)
	}

	var hostNames []string
	if containerApp.Properties != nil &&
		containerApp.Properties.Configuration != nil &&
		containerApp.Properties.Configuration.Ingress != nil &&
		containerApp.Properties.Configuration.Ingress.Fqdn != nil {
		hostNames = []string{*containerApp.Properties.Configuration.Ingress.Fqdn}
	} else {
		hostNames = []string{}
	}

	return &ContainerAppIngressConfiguration{
		HostNames: hostNames,
	}, nil
}

// apiVersionKey is the key that can be set in the root of a deployment yaml to control the API version used when creating
// or updating the container app. When unset, we use the default API version of the armappcontainers.ContainerAppsClient.
const apiVersionKey = "api-version"

func (cas *containerAppService) DeployYaml(
	ctx context.Context,
	subscriptionId string,
	resourceGroupName string,
	appName string,
	containerAppYaml []byte,
	progressLog func(string),
) error {
	var obj map[string]any
	if err := yaml.Unmarshal(containerAppYaml, &obj); err != nil {
		return fmt.Errorf("decoding yaml: %w", err)
	}

	var poller *runtime.Poller[armappcontainers.ContainerAppsClientCreateOrUpdateResponse]

	// The way we make the initial request depends on whether the apiVersion is specified in the YAML.
	if apiVersion, ok := obj[apiVersionKey].(string); ok {
		// When the apiVersion is specified, we need to use a custom policy to inject the apiVersion and body into the
		// request. This is because the ContainerAppsClient is built for a specific api version and does not allow us to
		// change it.  The custom policy allows us to use the parts of the SDK around building the request URL and using
		// the standard pipeline - but we have to use a policy to change the api-version header and inject the body since
		// the armappcontainers.ContainerApp{} is also built for a specific api version.
		customPolicy := &containerAppCustomApiVersionAndBodyPolicy{
			apiVersion: apiVersion,
		}

		appClient, err := cas.createContainerAppsClientWithPerCallPolicy(ctx, subscriptionId, customPolicy)
		if err != nil {
			return err
		}

		// Remove the apiVersion field from the object so it doesn't get injected into the request body. On the wire this
		// is in a query parameter, not the body.
		delete(obj, apiVersionKey)

		containerAppJson, err := json.Marshal(obj)
		if err != nil {
			panic("should not have failed")
		}

		// Set the body injected by the policy to be the full container app JSON from the YAML.
		customPolicy.body = (*json.RawMessage)(&containerAppJson)

		// It doesn't matter what we configure here - the value is going to be overwritten by the custom policy. But we need
		// to pass in a value, so use the zero value.
		emptyApp := armappcontainers.ContainerApp{}

		p, err := appClient.BeginCreateOrUpdate(ctx, resourceGroupName, appName, emptyApp, nil)
		if err != nil {
			return fmt.Errorf("applying manifest: %w", err)
		}
		poller = p

		// Now that we've sent the request, clear the body so it is not injected on any subsequent requests (e.g. ones made
		// by the poller when we poll).
		customPolicy.body = nil
	} else {
		// When the apiVersion field is unset in the YAML, we can use the standard SDK to build the request and send it
		// like normal.
		appClient, err := cas.createContainerAppsClient(ctx, subscriptionId)
		if err != nil {
			return err
		}

		containerAppJson, err := json.Marshal(obj)
		if err != nil {
			panic("should not have failed")
		}

		var containerApp armappcontainers.ContainerApp
		if err := json.Unmarshal(containerAppJson, &containerApp); err != nil {
			return fmt.Errorf("converting to container app type: %w", err)
		}

		p, err := appClient.BeginCreateOrUpdate(ctx, resourceGroupName, appName, containerApp, nil)
		if err != nil {
			return fmt.Errorf("applying manifest: %w", err)
		}

		poller = p
	}

	res, err := poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("polling for container app update completion: %w", err)
	}

	err = cas.waitForRevisionReady(
		ctx, subscriptionId, resourceGroupName, appName, *res.Properties.LatestRevisionName, progressLog)
	if err != nil {
		return err
	}

	return nil
}

// Adds and activates a new revision to the specified container app, by applying a new revision with only the image
// updated to the specified image name. The revision is waited on until it is in a running state.
func (cas *containerAppService) AddRevision(
	ctx context.Context,
	subscriptionId string,
	resourceGroupName string,
	appName string,
	imageName string,
	progress func(string),
) error {
	containerApp, err := cas.getContainerApp(ctx, subscriptionId, resourceGroupName, appName)
	if err != nil {
		return fmt.Errorf("getting container app: %w", err)
	}

	// Retrieve the latest revision name to perform a patch on
	currentRevisionName := *containerApp.Properties.LatestRevisionName
	revisionsClient, err := cas.createRevisionsClient(ctx, subscriptionId)
	if err != nil {
		return err
	}

	revisionResponse, err := revisionsClient.GetRevision(ctx, resourceGroupName, appName, currentRevisionName, nil)
	if err != nil {
		return fmt.Errorf("getting revision '%s': %w", currentRevisionName, err)
	}

	// Update the revision with the new image name and suffix
	revision := revisionResponse.Revision
	newRevisionSuffix := fmt.Sprintf("azd-%d", cas.clock.Now().Unix())
	// New revision name is always appName--revisionSuffix
	// see https://learn.microsoft.com/en-us/azure/container-apps/revisions#name-suffix
	newRevisionName := fmt.Sprintf("%s--%s", appName, newRevisionSuffix)
	revision.Properties.Template.RevisionSuffix = &newRevisionSuffix
	revision.Properties.Template.Containers[0].Image = convert.RefOf(imageName)

	// Update the container app with the new revision
	containerApp.Properties.Template = revision.Properties.Template
	containerApp, err = cas.syncSecrets(ctx, subscriptionId, resourceGroupName, appName, containerApp)
	if err != nil {
		return fmt.Errorf("syncing secrets: %w", err)
	}

	// Update the container app
	err = cas.updateContainerApp(ctx, subscriptionId, resourceGroupName, appName, containerApp)
	if err != nil {
		return fmt.Errorf("updating container app revision: %w", err)
	}

	err = cas.waitForRevisionReady(ctx, subscriptionId, resourceGroupName, appName, newRevisionName, progress)
	if err != nil {
		return err
	}

	// If the container app is in multiple revision mode, update the traffic to point to the new revision
	if *containerApp.Properties.Configuration.ActiveRevisionsMode == armappcontainers.ActiveRevisionsModeMultiple {
		err = cas.setTrafficWeights(ctx, subscriptionId, resourceGroupName, appName, containerApp, newRevisionName)
		if err != nil {
			return fmt.Errorf("setting traffic weights: %w", err)
		}
	}

	return nil
}

func (cas *containerAppService) waitForRevisionReady(
	ctx context.Context,
	subscriptionId string,
	resourceGroupName string,
	appName string,
	revisionName string,
	logProgress func(string)) error {
	statusPrefix := "Waiting for revision to be ready"
	logProgress(statusPrefix)

	credential, err := cas.credentialProvider.CredentialForSubscription(ctx, subscriptionId)
	if err != nil {
		return err
	}

	revisionsClient, err := armappcontainers.NewContainerAppsRevisionsClient(
		subscriptionId, credential, cas.armClientOptions)
	if err != nil {
		return fmt.Errorf("creating revisions client: %w", err)
	}

	replicasClient, err := armappcontainers.NewContainerAppsRevisionReplicasClient(
		subscriptionId, credential, cas.armClientOptions)
	if err != nil {
		return fmt.Errorf("creating replicas client: %w", err)
	}

	var rawResponse *http.Response
	getRevisionCtx := policy.WithCaptureResponse(ctx, &rawResponse)
	getRevisionCtx = policy.WithHTTPHeader(getRevisionCtx, httputil.PollHeader())

	// Poll for the revision to be in a running state
	// See https://learn.microsoft.com/en-us/azure/container-apps/revisions#lifecycle
	// for the complete lifecycle of a revision
	prevStatus := statusPrefix
	delay := 3 * time.Second
	pollCount := 0
	for {
		revision, err := revisionsClient.GetRevision(getRevisionCtx, resourceGroupName, appName, revisionName, nil)
		if err != nil {
			return fmt.Errorf("getting revision '%s': %w", revisionName, err)
		}

		if !*revision.Properties.Active {
			return fmt.Errorf("revision '%s' is not active", revisionName)
		}

		switch *revision.Properties.RunningState {
		// Failed states
		case armappcontainers.RevisionRunningStateFailed,
			armappcontainers.RevisionRunningStateStopped,
			armappcontainers.RevisionRunningStateDegraded:
			errSuffix := ""

			responseExtended := containerAppGetRevisionResponseExtended{}
			if err := runtime.UnmarshalAsJSON(rawResponse, &responseExtended); err == nil {
				if responseExtended.Properties.RunningStateDetails != "" {
					errSuffix = fmt.Sprintf(", %s", responseExtended.Properties.RunningStateDetails)
				}
			}

			revisionManagementUrl := output.WithLinkFormat(
				"%s/#@/resource/subscriptions/%s/resourceGroups/%s/providers/Microsoft.App/containerApps/%s/revisionManagement",
				string(cas.portalUrlBase),
				subscriptionId,
				resourceGroupName,
				appName)

			if v, err := strconv.ParseBool(os.Getenv("AZD_DEMO_MODE")); err == nil && v {
				revisionManagementUrl = fmt.Sprintf("Revision Management for %s in Azure Portal", appName)
			}

			return &internal.ErrorWithSuggestion{
				Err: fmt.Errorf("revision '%s' is in a %s state%s",
					revisionName,
					*revision.Properties.RunningState,
					errSuffix),
				Suggestion: "To view logs:" +
					"\n1. Visit " + revisionManagementUrl +
					"\n2. Click on revision " + fmt.Sprintf("'%s'", revisionName) +
					"\n3. View console and system logs" +
					"\nFor more troubleshooting information, visit " +
					output.WithLinkFormat("https://learn.microsoft.com/azure/container-apps/troubleshooting"),
			}
		// Processing states
		case armappcontainers.RevisionRunningStateProcessing, armappcontainers.RevisionRunningStateUnknown:
			// do nothing
		case armappcontainers.RevisionRunningStateRunning:
			// Success
			return nil
		}

		// Get replica information to provide additional status
		replicas, err := replicasClient.ListReplicas(ctx, resourceGroupName, appName, revisionName, nil)
		if err != nil {
			return fmt.Errorf("listing replicas: %w", err)
		}

		runningReplicas := 0
		readyReplicas := 0
		for _, r := range replicas.Value {
			ready := true
			if r.Properties.RunningState != nil &&
				*r.Properties.RunningState == armappcontainers.ContainerAppReplicaRunningStateRunning {
				runningReplicas++
			}

			for _, c := range r.Properties.Containers {
				if c.Ready == nil {
					ready = false
				}

				ready = ready && *c.Ready
			}

			if ready {
				readyReplicas++
			}
		}

		status := fmt.Sprintf(
			"%s: %d/%d replicas running, %d/%d replicas ready",
			statusPrefix,
			runningReplicas,
			len(replicas.Value),
			readyReplicas,
			len(replicas.Value))

		if status != prevStatus {
			logProgress(status)
			prevStatus = status
		}

		// Wait longer after a few initial tries
		if pollCount > 20 {
			delay = 10 * time.Second
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
			pollCount++
		}
	}
}

func (cas *containerAppService) ListSecrets(
	ctx context.Context,
	subscriptionId string,
	resourceGroupName string,
	appName string,
) ([]*armappcontainers.ContainerAppSecret, error) {
	appClient, err := cas.createContainerAppsClient(ctx, subscriptionId)
	if err != nil {
		return nil, err
	}

	secretsResponse, err := appClient.ListSecrets(ctx, resourceGroupName, appName, nil)
	if err != nil {
		return nil, fmt.Errorf("listing secrets: %w", err)
	}

	return secretsResponse.Value, nil
}

func (cas *containerAppService) syncSecrets(
	ctx context.Context,
	subscriptionId string,
	resourceGroupName string,
	appName string,
	containerApp *armappcontainers.ContainerApp,
) (*armappcontainers.ContainerApp, error) {
	// If the container app doesn't have any secrets, we don't need to do anything
	if len(containerApp.Properties.Configuration.Secrets) == 0 {
		return containerApp, nil
	}

	appClient, err := cas.createContainerAppsClient(ctx, subscriptionId)
	if err != nil {
		return nil, err
	}

	// Copy the secret configuration from the current version
	// Secret values are not returned by the API, so we need to get them separately
	// to ensure the update call succeeds
	secretsResponse, err := appClient.ListSecrets(ctx, resourceGroupName, appName, nil)
	if err != nil {
		return nil, fmt.Errorf("listing secrets: %w", err)
	}

	secrets := []*armappcontainers.Secret{}
	for _, secret := range secretsResponse.SecretsCollection.Value {
		secrets = append(secrets, &armappcontainers.Secret{
			Name:        secret.Name,
			Value:       secret.Value,
			Identity:    secret.Identity,
			KeyVaultURL: secret.KeyVaultURL,
		})
	}

	containerApp.Properties.Configuration.Secrets = secrets

	return containerApp, nil
}

func (cas *containerAppService) setTrafficWeights(
	ctx context.Context,
	subscriptionId string,
	resourceGroupName string,
	appName string,
	containerApp *armappcontainers.ContainerApp,
	revisionName string,
) error {
	containerApp.Properties.Configuration.Ingress.Traffic = []*armappcontainers.TrafficWeight{
		{
			RevisionName: &revisionName,
			Weight:       convert.RefOf[int32](100),
		},
	}

	err := cas.updateContainerApp(ctx, subscriptionId, resourceGroupName, appName, containerApp)
	if err != nil {
		return fmt.Errorf("updating traffic weights: %w", err)
	}

	return nil
}

func (cas *containerAppService) getContainerApp(
	ctx context.Context,
	subscriptionId string,
	resourceGroupName string,
	appName string,
) (*armappcontainers.ContainerApp, error) {
	appClient, err := cas.createContainerAppsClient(ctx, subscriptionId)
	if err != nil {
		return nil, err
	}

	containerAppResponse, err := appClient.Get(ctx, resourceGroupName, appName, nil)
	if err != nil {
		return nil, fmt.Errorf("getting container app: %w", err)
	}

	return &containerAppResponse.ContainerApp, nil
}

func (cas *containerAppService) updateContainerApp(
	ctx context.Context,
	subscriptionId string,
	resourceGroupName string,
	appName string,
	containerApp *armappcontainers.ContainerApp,
) error {
	appClient, err := cas.createContainerAppsClient(ctx, subscriptionId)
	if err != nil {
		return err
	}

	poller, err := appClient.BeginUpdate(ctx, resourceGroupName, appName, *containerApp, nil)
	if err != nil {
		return fmt.Errorf("begin updating ingress traffic: %w", err)
	}

	_, err = poller.PollUntilDone(ctx, nil)
	if err != nil {
		return fmt.Errorf("polling for container app update completion: %w", err)
	}

	return nil
}

func (cas *containerAppService) createContainerAppsClient(
	ctx context.Context,
	subscriptionId string,
) (*armappcontainers.ContainerAppsClient, error) {
	credential, err := cas.credentialProvider.CredentialForSubscription(ctx, subscriptionId)
	if err != nil {
		return nil, err
	}

	client, err := armappcontainers.NewContainerAppsClient(subscriptionId, credential, cas.armClientOptions)
	if err != nil {
		return nil, fmt.Errorf("creating ContainerApps client: %w", err)
	}

	return client, nil
}

func (cas *containerAppService) createContainerAppsClientWithPerCallPolicy(
	ctx context.Context,
	subscriptionId string,
	policy policy.Policy,
) (*armappcontainers.ContainerAppsClient, error) {
	credential, err := cas.credentialProvider.CredentialForSubscription(ctx, subscriptionId)
	if err != nil {
		return nil, err
	}

	// Clone the options so we don't modify the original - we don't want to inject this custom policy into every request.
	options := *cas.armClientOptions
	options.PerCallPolicies = append(slices.Clone(options.PerCallPolicies), policy)

	client, err := armappcontainers.NewContainerAppsClient(subscriptionId, credential, &options)
	if err != nil {
		return nil, fmt.Errorf("creating ContainerApps client: %w", err)
	}

	return client, nil
}

func (cas *containerAppService) createRevisionsClient(
	ctx context.Context,
	subscriptionId string,
) (*armappcontainers.ContainerAppsRevisionsClient, error) {
	credential, err := cas.credentialProvider.CredentialForSubscription(ctx, subscriptionId)
	if err != nil {
		return nil, err
	}

	client, err := armappcontainers.NewContainerAppsRevisionsClient(subscriptionId, credential, cas.armClientOptions)
	if err != nil {
		return nil, fmt.Errorf("creating ContainerApps client: %w", err)
	}

	return client, nil
}

type containerAppCustomApiVersionAndBodyPolicy struct {
	apiVersion string
	body       *json.RawMessage
}

func (p *containerAppCustomApiVersionAndBodyPolicy) Do(req *policy.Request) (*http.Response, error) {
	if p.body != nil {
		reqQP := req.Raw().URL.Query()
		reqQP.Set("api-version", p.apiVersion)
		req.Raw().URL.RawQuery = reqQP.Encode()

		log.Printf("setting body to %s", string(*p.body))

		if err := req.SetBody(streaming.NopCloser(bytes.NewReader(*p.body)), "application/json"); err != nil {
			return nil, fmt.Errorf("updating request body: %w", err)
		}
	}

	return req.Next()
}

type containerAppGetRevisionResponseExtended struct {
	Properties containerAppGetRevisionResponsePropertiesExtended
}

type containerAppGetRevisionResponsePropertiesExtended struct {
	RunningStateDetails string
}
