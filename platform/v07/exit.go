package v07

import "github.com/buildpacks/lifecycle/platform/common"

func (p *Platform) CodeFor(errType common.LifecycleExitError) int {
	return p.previousPlatform.CodeFor(errType)
}