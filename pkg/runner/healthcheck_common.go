package runner

import (
	"context"
	"fmt"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/docker/go-units"
	"github.com/ipfs/testground/pkg/api"
	"github.com/ipfs/testground/pkg/docker"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

type Checker func() bool
type Fixer func() error

// HealthcheckHelper is a strategy interface for runners.
// Each runner may have required elements -- infrastructure, etc. which should be checked prior to
// running plans. Individual checks are registered to the HealthcheckHelper using the Enlist()
// method. With all of the checks enlisted, execute the checks, and optionally fixes, using the
// RunChecks() method. The details of how the checks are performed is implementation specific.
// Typically, the checker and fixer passed to the enlist method will be closures. These methods will
// be called when RunChecks is executed.
type HealthcheckHelper interface {
	Enlist(name string, c Checker, f Fixer)
	RunChecks(ctx context.Context, fix bool) *api.HealthcheckReport
}

type toDoElement struct {
	Name    string
	Checker Checker
	Fixer   Fixer
}

// ErrGroupHealthcheckHelper implements HealthcheckHelper using an errgroup for paralellism.
type ErrgroupHealthcheckHelper struct {
	toDo   []*toDoElement
	report *api.HealthcheckReport
}

func (hh *ErrgroupHealthcheckHelper) Enlist(name string, c Checker, f Fixer) {
	hh.toDo = append(hh.toDo, &toDoElement{name, c, f})
}

func (hh *ErrgroupHealthcheckHelper) RunChecks(ctx context.Context, fix bool) *api.HealthcheckReport {
	eg, _ := errgroup.WithContext(ctx)

	for _, li := range hh.toDo {
		hcp := *li
		eg.Go(func() error {
			// Checker succeeds, already working.
			if hcp.Checker() {
				hh.report.Checks = append(hh.report.Checks, api.HealthcheckItem{
					Name:    li.Name,
					Status:  api.HealthcheckStatusOK,
					Message: fmt.Sprintf("%s: OK", li.Name),
				})
				return nil
			}
			// Checker failed, Append the failure to the check report
			hh.report.Checks = append(hh.report.Checks, api.HealthcheckItem{
				Name:    li.Name,
				Status:  api.HealthcheckStatusFailed,
				Message: fmt.Sprintf("%s: FAILED. Fixing: %t", li.Name, fix),
			})
			// Attempt fix if fix is enabled.
			fixhc := api.HealthcheckItem{Name: li.Name}
			if fix {
				err := li.Fixer()
				if err != nil {
					// Oh no! the fix failed.
					fixhc.Status = api.HealthcheckStatusFailed
					fixhc.Message = fmt.Sprintf("%s FAILED: %v", li.Name, err)
				} else {
					// Fix succeeded.
					fixhc.Status = api.HealthcheckStatusOK
					fixhc.Message = fmt.Sprintf("%s RECOVERED", li.Name)
				}
			} else {
				// don't attempt to fix.
				fixhc.Status = api.HealthcheckStatusOmitted
				fixhc.Message = fmt.Sprintf("%s recovery not attempted.", li.Name)
			}
			// Fill the report with fix information.
			hh.report.Fixes = append(hh.report.Fixes, fixhc)
			return nil
		})
		eg.Wait()
	}
	return hh.report
}

// DefaultContainerChecker returns a Checker, a method which when executed will check for the
// existance of the container. This should be considered a sensible default for checking whether
// docker containers are started.
func DefaultContainerChecker(ctx context.Context, log *zap.SugaredLogger, cli *client.Client, name string) Checker {
	return func() bool {
		ci, err := docker.CheckContainer(ctx, log, cli, name)
		if err != nil || ci == nil {
			return false
		}
		return ci.State.Running

	}
}

// DefaultContainerFixer returns a Fixer, a method which when executed will ensure the named
// container exists with some default paramaters which are appropriate for infra containers.
// Unless containers require special consideration, this should be considered the sensible default
// fixer for docker containers.
func DefaultContainerFixer(ctx context.Context, log *zap.SugaredLogger, cli *client.Client, containerName string, imageName string, networkID string, portSpecs []string, pull bool, cmds ...string) Fixer {
	// Docker host configuration.
	// https://godoc.org/github.com/docker/docker/api/types/container#HostConfig
	hostConfig := container.HostConfig{
		NetworkMode: container.NetworkMode(networkID),
		Resources: container.Resources{
			Ulimits: []*units.Ulimit{
				{Name: "nofile", Hard: InfraMaxFilesUlimit, Soft: InfraMaxFilesUlimit},
			},
		},
	}
	// Try to parse the portSpecs, but if we can't, fall back to using random host port assignments.
	// the portSpec should be in the format ip:public:private/proto
	_, portBindings, err := nat.ParsePortSpecs(portSpecs)
	if err != nil {
		hostConfig.PublishAllPorts = true
	} else {
		hostConfig.PortBindings = portBindings
	}

	// Configuration for the container:
	containerConfig := container.Config{
		Image: imageName,
		Cmd:   cmds,
	}

	ensure := docker.EnsureContainerOpts{
		ContainerName:      containerName,
		ContainerConfig:    &containerConfig,
		HostConfig:         &hostConfig,
		PullImageIfMissing: pull,
	}

	// Make sure this container is running when the closure is executed.
	return func() error {
		_, _, err := docker.EnsureContainer(ctx, log, cli, &ensure)
		return err
	}
}