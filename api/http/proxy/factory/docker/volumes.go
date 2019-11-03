package docker

import (
	"context"
	"net/http"

	"github.com/docker/docker/client"

	"github.com/portainer/portainer/api"
	"github.com/portainer/portainer/api/http/proxy/factory/responseutils"
)

const (
	errDockerVolumeIdentifierNotFound = portainer.Error("Docker volume identifier not found")
	volumeIdentifier                  = "Name"
	volumeLabelForStackIdentifier     = "com.docker.stack.namespace"
)

func getInheritedResourceControlFromVolumeLabels(dockerClient *client.Client, volumeID string, resourceControls []portainer.ResourceControl) (*portainer.ResourceControl, error) {
	network, err := dockerClient.VolumeInspect(context.Background(), volumeID)
	if err != nil {
		return nil, err
	}

	swarmStackName := network.Labels[volumeLabelForStackIdentifier]
	if swarmStackName != "" {
		return portainer.GetResourceControlByResourceIDAndType(swarmStackName, portainer.StackResourceControl, resourceControls), nil
	}

	return nil, nil
}

// volumeListOperation extracts the response as a JSON object, loop through the volume array
// decorate and/or filter the volumes based on resource controls before rewriting the response
func volumeListOperation(response *http.Response, executor *operationExecutor) error {
	var err error
	// VolumeList response is a JSON object
	// https://docs.docker.com/engine/api/v1.28/#operation/VolumeList
	responseObject, err := responseutils.GetResponseAsJSONOBject(response)
	if err != nil {
		return err
	}

	// The "Volumes" field contains the list of volumes as an array of JSON objects
	// Response schema reference: https://docs.docker.com/engine/api/v1.28/#operation/VolumeList
	if responseObject["Volumes"] != nil {
		volumeData := responseObject["Volumes"].([]interface{})

		if executor.operationContext.isAdmin || executor.operationContext.endpointResourceAccess {
			volumeData, err = decorateVolumeList(volumeData, executor.operationContext.resourceControls)
		} else {
			volumeData, err = filterVolumeList(volumeData, executor.operationContext)
		}
		if err != nil {
			return err
		}

		// Overwrite the original volume list
		responseObject["Volumes"] = volumeData
	}

	return responseutils.RewriteResponse(response, responseObject, http.StatusOK)
}

// volumeInspectOperation extracts the response as a JSON object, verify that the user
// has access to the volume based on any existing resource control and either rewrite an access denied response
// or a decorated volume.
func volumeInspectOperation(response *http.Response, executor *operationExecutor) error {
	// VolumeInspect response is a JSON object
	// https://docs.docker.com/engine/api/v1.28/#operation/VolumeInspect
	responseObject, err := responseutils.GetResponseAsJSONOBject(response)
	if err != nil {
		return err
	}

	if responseObject[volumeIdentifier] == nil {
		return errDockerVolumeIdentifierNotFound
	}

	resourceControl := findInheritedVolumeResourceControl(responseObject, executor.operationContext.resourceControls)
	if resourceControl == nil && (executor.operationContext.isAdmin || executor.operationContext.endpointResourceAccess) {
		return responseutils.RewriteResponse(response, responseObject, http.StatusOK)
	}

	if executor.operationContext.isAdmin || executor.operationContext.endpointResourceAccess || portainer.UserCanAccessResource(executor.operationContext.userID, executor.operationContext.userTeamIDs, resourceControl) {
		responseObject = decorateObject(responseObject, resourceControl)
		return responseutils.RewriteResponse(response, responseObject, http.StatusOK)
	}

	return responseutils.RewriteAccessDeniedResponse(response)
}

// findInheritedVolumeResourceControl will search for a resource control object associated to the service or
// inherited from a Swarm stack (based on labels).
func findInheritedVolumeResourceControl(responseObject map[string]interface{}, resourceControls []portainer.ResourceControl) *portainer.ResourceControl {
	volumeID := responseObject[volumeIdentifier].(string)

	resourceControl := portainer.GetResourceControlByResourceIDAndType(volumeID, portainer.VolumeResourceControl, resourceControls)
	if resourceControl != nil {
		return resourceControl
	}

	volumeLabels := extractVolumeLabelsFromVolumeInspectObject(responseObject)
	if volumeLabels != nil {
		if volumeLabels[volumeLabelForStackIdentifier] != nil {
			inheritedSwarmStackIdentifier := volumeLabels[volumeLabelForStackIdentifier].(string)
			resourceControl = portainer.GetResourceControlByResourceIDAndType(inheritedSwarmStackIdentifier, portainer.StackResourceControl, resourceControls)

			if resourceControl != nil {
				return resourceControl
			}
		}
	}

	return nil
}

// extractVolumeLabelsFromVolumeInspectObject retrieve the Labels of the volume if present.
// Volume schema reference: https://docs.docker.com/engine/api/v1.28/#operation/VolumeInspect
func extractVolumeLabelsFromVolumeInspectObject(responseObject map[string]interface{}) map[string]interface{} {
	// Labels are stored under Labels
	return responseutils.GetJSONObject(responseObject, "Labels")
}

// extractVolumeLabelsFromVolumeListObject retrieve the Labels of the volume if present.
// Volume schema reference: https://docs.docker.com/engine/api/v1.28/#operation/VolumeList
func extractVolumeLabelsFromVolumeListObject(responseObject map[string]interface{}) map[string]interface{} {
	// Labels are stored under Labels
	return responseutils.GetJSONObject(responseObject, "Labels")
}

// decorateVolumeList loops through all volumes and decorates any volume with an existing resource control.
// Resource controls checks are based on: resource identifier, stack identifier (from label).
// Volume object schema reference: https://docs.docker.com/engine/api/v1.28/#operation/VolumeList
func decorateVolumeList(volumeData []interface{}, resourceControls []portainer.ResourceControl) ([]interface{}, error) {
	decoratedVolumeData := make([]interface{}, 0)

	for _, volume := range volumeData {

		volumeObject := volume.(map[string]interface{})
		if volumeObject[volumeIdentifier] == nil {
			return nil, errDockerVolumeIdentifierNotFound
		}

		volumeID := volumeObject[volumeIdentifier].(string)
		volumeObject = decorateResourceWithAccessControl(volumeObject, volumeID, resourceControls, portainer.VolumeResourceControl)

		volumeLabels := extractVolumeLabelsFromVolumeListObject(volumeObject)
		volumeObject = decorateResourceWithAccessControlFromLabel(volumeLabels, volumeObject, volumeLabelForStackIdentifier, resourceControls, portainer.StackResourceControl)

		decoratedVolumeData = append(decoratedVolumeData, volumeObject)
	}

	return decoratedVolumeData, nil
}

// filterVolumeList loops through all volumes and filters authorized volumes (access granted to the user based on existing resource control).
// Authorized volumes are decorated during the process.
// Resource controls checks are based on: resource identifier, stack identifier (from label).
// Volume object schema reference: https://docs.docker.com/engine/api/v1.28/#operation/VolumeList
func filterVolumeList(volumeData []interface{}, context *restrictedDockerOperationContext) ([]interface{}, error) {
	filteredVolumeData := make([]interface{}, 0)

	for _, volume := range volumeData {
		volumeObject := volume.(map[string]interface{})
		if volumeObject[volumeIdentifier] == nil {
			return nil, errDockerVolumeIdentifierNotFound
		}

		volumeID := volumeObject[volumeIdentifier].(string)
		volumeObject, access := applyResourceAccessControl(volumeObject, volumeID, context, portainer.VolumeResourceControl)
		if !access {
			volumeLabels := extractVolumeLabelsFromVolumeListObject(volumeObject)
			volumeObject, access = applyResourceAccessControlFromLabel(volumeLabels, volumeObject, volumeLabelForStackIdentifier, context, portainer.StackResourceControl)
		}

		if access {
			filteredVolumeData = append(filteredVolumeData, volumeObject)
		}
	}

	return filteredVolumeData, nil
}
