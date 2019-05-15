// +build windows

package driver

import (
	"fmt"
	"os"
	"strings"
)

// CheckDriverExists checks to see if the named driver exists
func (d *CommandDriver) CheckDriverExists() bool {

	path := os.Getenv("PATH")
	extensions := [3]string{"exe", "cmd", "bat"}
	for _, dir := range strings.Split(path, ";") {
		for _, ext := range extensions {
			filename := fmt.Sprintf("%s/%s.%s", dir, d.cliName(), ext)
			if _, err := os.Stat(filename); err == nil {
				return true
			}

		}
	}
	return false
}
