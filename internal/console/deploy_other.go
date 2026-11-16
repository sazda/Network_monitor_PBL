//go:build !windows

package console

import "context"

func defaultDeployRunner(string) deployRunner {
	return func(context.Context, deployTarget) deployOutcome {
		return deployOutcome{Message: "instalacija agenata iz konzole je dostupna samo kada konzola radi na Windows-u"}
	}
}
