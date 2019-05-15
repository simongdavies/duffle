package driver

import (
	"fmt"
	"io/ioutil"
	"os"
	"runtime"
	"testing"

	"github.com/deislabs/cnab-go/driver"
)

var _ driver.Driver = &CommandDriver{}

func TestCheckDriverExists(t *testing.T) {
	name := "missing-driver"
	cmddriver := &CommandDriver{Name: name}
	if cmddriver.CheckDriverExists() {
		t.Errorf("Expected driver %s not to exist", name)
	}

	name = "existing-driver"
	path_seperator := ":"
	cmddriver = &CommandDriver{Name: name}

	if runtime.GOOS == "windows" {
		name = fmt.Sprintf("%s.exe", name)
		path_seperator = ";"
	}

	dirname, err := ioutil.TempDir("", "duffle")
	if err != nil {
		t.Fatal(err)
	}

	defer os.RemoveAll(dirname)
	filename := fmt.Sprintf("%s/duffle-%s", dirname, name)
	newfile, err := os.Create(filename)
	if err != nil {
		t.Fatal(err)
	}

	newfile.Chmod(0755)
	path := os.Getenv("PATH")
	newpath := fmt.Sprintf("%s%s%s", dirname, path_seperator, path)
	defer os.Setenv("PATH", path)
	os.Setenv("PATH", newpath)
	if !cmddriver.CheckDriverExists() {
		t.Errorf("Expected driver %s to exist", name)
	}

}
