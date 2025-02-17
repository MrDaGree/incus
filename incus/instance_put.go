package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"

	"github.com/gorilla/mux"
	"github.com/pborman/uuid"

	"github.com/cyphar/incus/incus/db"
	"github.com/cyphar/incus/incus/db/cluster"
	"github.com/cyphar/incus/incus/db/operationtype"
	deviceConfig "github.com/cyphar/incus/incus/device/config"
	"github.com/cyphar/incus/incus/instance"
	"github.com/cyphar/incus/incus/instance/instancetype"
	"github.com/cyphar/incus/incus/operations"
	projecthelpers "github.com/cyphar/incus/incus/project"
	"github.com/cyphar/incus/incus/response"
	"github.com/cyphar/incus/incus/state"
	"github.com/cyphar/incus/incus/util"
	"github.com/cyphar/incus/shared"
	"github.com/cyphar/incus/shared/api"
	"github.com/cyphar/incus/shared/osarch"
	"github.com/cyphar/incus/shared/version"
)

// swagger:operation PUT /1.0/instances/{name} instances instance_put
//
//	Update the instance
//
//	Updates the instance configuration or trigger a snapshot restore.
//
//	---
//	consumes:
//	  - application/json
//	produces:
//	  - application/json
//	parameters:
//	  - in: query
//	    name: project
//	    description: Project name
//	    type: string
//	    example: default
//	  - in: body
//	    name: instance
//	    description: Update request
//	    schema:
//	      $ref: "#/definitions/InstancePut"
//	responses:
//	  "202":
//	    $ref: "#/responses/Operation"
//	  "400":
//	    $ref: "#/responses/BadRequest"
//	  "403":
//	    $ref: "#/responses/Forbidden"
//	  "500":
//	    $ref: "#/responses/InternalServerError"
func instancePut(d *Daemon, r *http.Request) response.Response {
	// Don't mess with instance while in setup mode.
	<-d.waitReady.Done()

	s := d.State()

	instanceType, err := urlInstanceTypeDetect(r)
	if err != nil {
		return response.SmartError(err)
	}

	projectName := projectParam(r)

	// Get the container
	name, err := url.PathUnescape(mux.Vars(r)["name"])
	if err != nil {
		return response.SmartError(err)
	}

	if shared.IsSnapshot(name) {
		return response.BadRequest(fmt.Errorf("Invalid instance name"))
	}

	// Handle requests targeted to a container on a different node
	resp, err := forwardedResponseIfInstanceIsRemote(s, r, projectName, name, instanceType)
	if err != nil {
		return response.SmartError(err)
	}

	if resp != nil {
		return resp
	}

	inst, err := instance.LoadByProjectAndName(s, projectName, name)
	if err != nil {
		return response.SmartError(err)
	}

	// Validate the ETag
	etag := []any{inst.Architecture(), inst.LocalConfig(), inst.LocalDevices(), inst.IsEphemeral(), inst.Profiles()}
	err = util.EtagCheck(r, etag)
	if err != nil {
		return response.PreconditionFailed(err)
	}

	configRaw := api.InstancePut{}
	err = json.NewDecoder(r.Body).Decode(&configRaw)
	if err != nil {
		return response.BadRequest(err)
	}

	architecture, err := osarch.ArchitectureId(configRaw.Architecture)
	if err != nil {
		architecture = 0
	}

	var do func(*operations.Operation) error
	var opType operationtype.Type
	if configRaw.Restore == "" {
		// Check project limits.
		apiProfiles := make([]api.Profile, 0, len(configRaw.Profiles))
		err = s.DB.Cluster.Transaction(r.Context(), func(ctx context.Context, tx *db.ClusterTx) error {
			profiles, err := cluster.GetProfilesIfEnabled(ctx, tx.Tx(), projectName, configRaw.Profiles)
			if err != nil {
				return err
			}

			for _, profile := range profiles {
				apiProfile, err := profile.ToAPI(ctx, tx.Tx())
				if err != nil {
					return err
				}

				apiProfiles = append(apiProfiles, *apiProfile)
			}

			return projecthelpers.AllowInstanceUpdate(tx, projectName, name, configRaw, inst.LocalConfig())
		})
		if err != nil {
			return response.SmartError(err)
		}

		// Update container configuration
		do = func(op *operations.Operation) error {
			args := db.InstanceArgs{
				Architecture: architecture,
				Config:       configRaw.Config,
				Description:  configRaw.Description,
				Devices:      deviceConfig.NewDevices(configRaw.Devices),
				Ephemeral:    configRaw.Ephemeral,
				Profiles:     apiProfiles,
				Project:      projectName,
			}

			err = inst.Update(args, true)
			if err != nil {
				return err
			}

			return nil
		}

		opType = operationtype.InstanceUpdate
	} else {
		// Snapshot Restore
		do = func(op *operations.Operation) error {
			return instanceSnapRestore(s, projectName, name, configRaw.Restore, configRaw.Stateful)
		}

		opType = operationtype.SnapshotRestore
	}

	resources := map[string][]api.URL{}
	resources["instances"] = []api.URL{*api.NewURL().Path(version.APIVersion, "instances", name)}

	if inst.Type() == instancetype.Container {
		resources["containers"] = resources["instances"]
	}

	op, err := operations.OperationCreate(s, projectName, operations.OperationClassTask, opType, resources, nil, do, nil, nil, r)
	if err != nil {
		return response.InternalError(err)
	}

	return operations.OperationResponse(op)
}

func instanceSnapRestore(s *state.State, projectName string, name string, snap string, stateful bool) error {
	// normalize snapshot name
	if !shared.IsSnapshot(snap) {
		snap = name + shared.SnapshotDelimiter + snap
	}

	inst, err := instance.LoadByProjectAndName(s, projectName, name)
	if err != nil {
		return err
	}

	source, err := instance.LoadByProjectAndName(s, projectName, snap)
	if err != nil {
		switch {
		case response.IsNotFoundError(err):
			return fmt.Errorf("Snapshot %s does not exist", snap)
		default:
			return err
		}
	}

	// Generate a new `volatile.uuid.generation` to differentiate this instance restored from a snapshot from the original instance.
	source.LocalConfig()["volatile.uuid.generation"] = uuid.New()

	err = inst.Restore(source, stateful)
	if err != nil {
		return err
	}

	return nil
}
