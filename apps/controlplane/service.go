// `varde-control-plane service install|uninstall|run`: Windows service
// management. The SCM-invoked `service run` path shares the flag set and the
// serve() implementation with interactive use.
package main

import (
	"fmt"
	"os"
)

const (
	serviceName    = "VardeControlPlane"
	serviceDisplay = "Varde Control Plane"
)

func serviceCmd(args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: varde-control-plane service install|uninstall|run")
		return 2
	}
	var err error
	switch args[0] {
	case "install":
		err = serviceInstall()
	case "uninstall":
		err = serviceUninstall()
	case "run":
		err = serviceRun(args[1:])
	default:
		fmt.Fprintf(os.Stderr, "unknown service action %q\n", args[0])
		return 2
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}
