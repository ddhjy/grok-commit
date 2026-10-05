package commit

import (
	"errors"
	"os/exec"
)

func detach(cmd *exec.Cmd) {}
func lockWorker(string) (func(), error) {
	return nil, errors.New("background service is not available on Windows")
}
