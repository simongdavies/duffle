package driver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	yaml "gopkg.in/yaml.v2"

	"github.com/google/uuid"
)

// ACIDriver runs Docker invocation images in ACI
type ACIDriver struct {
	config               map[string]string
	inCloudShell         bool
	delete_aci_resources bool
}

// Config returns the ACI driver configuration options
func (d *ACIDriver) Config() map[string]string {
	return map[string]string{
		"ARM_CLIENT_ID":       "AAD Client ID for Azure account authentication - used for ACI creation",
		"ARM_CLIENT_SECRET":   "AAD Client Secret for Azure account authentication - used for ACI creation",
		"ARM_TENANT_ID":       "Azure AAD Tenant Id Azure account authentication - used for ACI creation",
		"ARM_SUBSCRIPTION_ID": "Azure Subscription Id - this is the subscription to be used for ACI creation, if not specified the default subscription is used",
		"ACI_RESOURCE_GROUP":  "The name of the Resource Group to create the ACI instance in",
		"ACI_LOCATION":        "The location to create the ACI Instance in",
		"ACI_NAME":            "The name of the ACI instance to create",
		"ACI_DO_NOT_DELETE":   "Do not delete RG and ACI instance created",
	}
}

// SetConfig sets ACI driver configuration
func (d *ACIDriver) SetConfig(settings map[string]string) {
	d.config = settings
}

// Run executes the ACI driver
func (d *ACIDriver) Run(op *Operation) error {
	return d.exec(op)
}

// Handles indicates that the ACI driver supports "docker" and "oci"
func (d *ACIDriver) Handles(dt string) bool {
	return dt == ImageTypeDocker || dt == ImageTypeOCI
}

// this uses az cli as the go sdk is incomplete for ACI
// possibly only missing the ability to stream logs so should probably change this
func (d *ACIDriver) exec(op *Operation) error {

	d.delete_aci_resources = true

	if len(d.config["ACI_DO_NOT_DELETE"]) > 0 && strings.ToLower(d.config["ACI_DO_NOT_DELETE"]) == "true" {
		d.delete_aci_resources = false
	}

	d.inCloudShell = len(os.Getenv("ACC_CLOUD")) > 0

	if d.inCloudShell {
		fmt.Println("Running in CloudShell")
		handleCtrlC(logInWithSystemMSI)
		defer logInWithSystemMSI()
	} else {

		// TODO: check if az cli is installed

		loggedIn, err := checkIfLoggedInToAzure()
		if err != nil {
			return fmt.Errorf("cannot check if Logged in To Azure: %v", err)
		}
		if loggedIn {
			fmt.Println("Already Logged in to Azure - logging out")
			logoutOfAzure()
		}
		// Log out at the end
		handleCtrlC(logoutOfAzure)
		defer logoutOfAzure()
	}

	err := d.loginToAzure()
	if err != nil {
		return fmt.Errorf("cannot Login To Azure: %v", err)
	}

	err = d.setCurrentAzureSubscription()
	if err != nil {
		return fmt.Errorf("cannot set Azure subscription: %v", err)
	}

	err = d.createACIInstance(op)
	if err != nil {
		return fmt.Errorf("creating ACI instance failed: %v", err)
	}

	return nil
}

func handleCtrlC(f func() error) {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	go func() {
		for range c {
			f()
		}
	}()
}

func checkIfLoggedInToAzure() (bool, error) {

	err := runCommandNoStderr("az", "account", "show")
	if err != nil {

		returnCode, e := getCommandReturnCode(err)

		// Cant get the return code

		if e != nil {
			return false, fmt.Errorf("Cannot get return code from command: %v", e)
		}

		// Not Logged in

		if returnCode != 0 {
			return false, nil
		}

	}

	return true, nil
}
func logbackinCloudShell() error {
	fmt.Println("Logging out of Azure")
	err := runCommand("az", "logout")
	if err != nil {
		return fmt.Errorf("Attempt to logout of Azure failed: %v", err)
	}
	return runCommand("az", "login", "--identity")
}
func logoutOfAzure() error {
	fmt.Println("Logging out of Azure")
	return runCommand("az", "logout")
}

func (d *ACIDriver) loginToAzure() error {

	// Check if SPN

	userName := d.config["ARM_CLIENT_ID"]
	password := d.config["ARM_CLIENT_SECRET"]
	tenant := d.config["ARM_TENANT_ID"]

	if len(userName) != 0 && len(password) != 0 && len(tenant) != 0 {

		fmt.Println("Attempting to Login with Service Principal")

		err := runCommand("az", "login", "--service-principal", "--tenant", tenant, "--username", userName, "--password", password)
		if err != nil {
			return fmt.Errorf("Attempt to login to azure with SPN failed: %v", err)
		}

		return nil
	}

	// Check for User MSI and if Token Issuing endpoint available

	userMSI := d.config["ARM_USER_MSI"]
	hasMSIEndpoint := checkForMSIEndpoint()

	if len(userMSI) != 0 && hasMSIEndpoint {

		fmt.Println("Attempting to Login with User MSI")

		err := runCommand("az", "login", "--identity", "--username", userMSI)
		if err != nil {
			return fmt.Errorf("Attempt to login to azure ith UserMSI failed: %v", err)
		}

		return nil

	}

	// check for CloudShell or System MSI

	if d.inCloudShell || hasMSIEndpoint {

		err := logInWithSystemMSI()
		if err != nil {
			if d.inCloudShell {
				return fmt.Errorf("Attempt to login to azure with CloudShell failed: %v", err)
			}
			return fmt.Errorf("Attempt to login to azure with SystemMSI failed: %v", err)
		}

		return nil

	}

	return errors.New("Cannot login to Azure - no valid credentials provided")

}

func logInWithSystemMSI() error {

	return runCommand("az", "login", "--identity")

}

func (d *ACIDriver) setCurrentAzureSubscription() error {

	subscription := d.config["ARM_SUBSCRIPTION_ID"]

	if len(subscription) != 0 {
		fmt.Printf("Setting Subscripiton to %s\n", subscription)
		return runCommand("az", "account", "set", "--subscription", subscription)
	}

	return nil

}

func (d *ACIDriver) createACIInstance(op *Operation) error {

	// GET ACI Config

	aciRG := d.config["ACI_RESOURCE_GROUP"]
	aciLocation := d.config["ACI_LOCATION"]

	if len(aciLocation) > 0 {
		cmd := exec.Command("az", "provider", "show", "--namespace", "Microsoft.ContainerInstance", "--query", "resourceTypes[0].locations")
		resp, err := cmd.Output()
		if err != nil {
			return fmt.Errorf("Error getting locations for ACI: %v", err)
		}
		var locations []string
		err = json.Unmarshal(resp, &locations)
		if err != nil {
			return fmt.Errorf("Error unmarshalling location list for ACI: %v", err)
		}
		if !locationIsAvailable(aciLocation, locations) {
			return fmt.Errorf("ACI Location is invalid: %v", aciLocation)
		}
	} else if len(aciRG) == 0 {
		return errors.New("ACI Driver requires ACI_LOCATION environment variable or an existing Resource Group in ACI_RESOURE_GROUP")
	}

	if len(aciRG) == 0 {
		aciRG = uuid.New().String()
		err := runCommand("az", "group", "create", "--name", aciRG, "--location", aciLocation)
		if err != nil {
			return fmt.Errorf("Failed to create resource group: %v", err)
		}
		defer func() {
			if d.delete_aci_resources {
				err = runCommand("az", "group", "delete", "--name", aciRG, "--yes")
				if err != nil {
					fmt.Fprintf(os.Stderr, "Failed to delete resource group %s error: %v\n", aciRG, err)
				}
			}
		}()
	} else {
		err := runCommand("az", "group", "show", "--name", aciRG)
		if err != nil {
			returnCode, e := getCommandReturnCode(err)

			// Cant get the return code
			if e != nil {
				return fmt.Errorf("Cannot get return code from command az group show: %v", e)
			}

			// Not Logged in
			if returnCode == 3 {
				return fmt.Errorf("Resource Group does not exist: %v", e)
			}

			return fmt.Errorf("Checking for existing resource group %s failed with return code:%d error: %v", aciRG, returnCode, err)
		}
	}

	aciName := d.config["ACI_NAME"]
	if len(aciName) == 0 {
		aciName = fmt.Sprintf("duffle-%s", uuid.New().String())
	}

	// Does not support files

	if len(op.Files) > 0 {
		// TODO copy files to an Azure File Share
		return errors.New("ACI Driver does not support files")
	}

	// Create Container

	var env []environmentVariableDef
	for k, v := range op.Environment {
		env = append(env, environmentVariableDef{k, strings.Replace(v, "'", "''", -1)})
	}

	fileName, err := createContainerGroupYAMLFile(aciName, aciLocation, op.Image, env)
	if err != nil {
		return fmt.Errorf("Failed to create YAML config file:%v", err)
	}

	defer os.Remove(fileName)

	fmt.Println("Creating Azure Container Instance")

	err = runCommand("az", "container", "create", "--resource-group", aciRG, "--file", fileName)
	if err != nil {
		return fmt.Errorf("Error creating ACI Instance:%v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error)
	go func(ctx context.Context, cancel context.CancelFunc) {

		defer func() {
			if d.delete_aci_resources {
				fmt.Println("Deleting Container Instance")
				err = runCommand("az", "container", "delete", "--resource-group", aciRG, "--name", aciName, "--yes")
				if err != nil {
					errc <- err
					return
				}
			}
			close(errc)
		}()

		containerRunning := true
		var err error
		for containerRunning {

			cmd := exec.Command("az", "container", "show", "--resource-group", aciRG, "--name", aciName, "--query", "instanceView.state")
			resp, err := cmd.Output()
			if err != nil {
				break
			}

			output := strings.Trim(strings.TrimSpace(string(resp)), "\"")

			if strings.Compare(output, "Running") == 0 {
				time.Sleep(5 * time.Second)
			} else {

				if strings.Compare(output, "Succeeded") != 0 {
					err = fmt.Errorf("Unexpected Container Status:%s", output)
				} else {
					fmt.Println("Container terminated successfully")
				}
				containerRunning = false
			}

		}

		if err != nil {
			errc <- err
			return
		}

		// cancel log output

		cancel()

		errc <- nil

	}(ctx, cancel)

	// Get Logs

	fmt.Println("Getting Logs")

	err = runCommandWithContext(ctx, true, "az", "container", "logs", "--resource-group", aciRG, "--name", aciName, "--container-name", aciName, "--follow")

	if err != nil {
		if ctx.Err() != context.Canceled {
			return err
		}
	}

	switch len(errc) {

	case 0:
		fmt.Println("Done")
		return nil
	case 1:
		return err
	default:
		{
			numErrors := len(errc)
			fmt.Printf("%d errors occurred\n", numErrors)
			for err := range errc {
				fmt.Printf("Error Occurred: %v\n", err)
			}
			return fmt.Errorf("%d Errors Occurred", numErrors)
		}
	}

}
func locationIsAvailable(location string, locations []string) bool {

	location = strings.ToLower(strings.Replace(location, " ", "", -1))
	for _, l := range locations {
		l = strings.ToLower(strings.Replace(l, " ", "", -1))
		if l == location {
			return true
		}
	}
	return false
}
func checkForMSIEndpoint() bool {

	timeout := time.Duration(1 * time.Second)
	client := http.Client{
		Timeout: timeout,
	}

	_, err := client.Head("http://169.254.169.254/metadata/identity/oauth2/token")
	if err != nil {
		return false
	}

	return true

}
func runCommand(command string, args ...string) error {

	return runCommandWithContext(context.Background(), true, command, args...)

}
func runCommandNoStderr(command string, args ...string) error {

	return runCommandWithContext(context.Background(), false, command, args...)

}
func runCommandWithContext(ctx context.Context, outputStderr bool, command string, args ...string) error {

	cmd := exec.CommandContext(ctx, command, args...)

	stdout, _ := cmd.StdoutPipe()
	go func() {
		io.Copy(os.Stdout, stdout)
	}()

	if outputStderr {
		stderr, _ := cmd.StderrPipe()

		go func() {
			io.Copy(os.Stderr, stderr)
		}()
	}

	err := cmd.Start()
	if err != nil {
		return fmt.Errorf("cmd.Start(%s) failed: %v", command, err)
	}

	err = cmd.Wait()
	if err != nil {
		return err
	}

	return nil

}
func getCommandReturnCode(err error) (int, error) {
	if exitError, ok := err.(*exec.ExitError); ok {
		ws := exitError.Sys().(syscall.WaitStatus)
		return ws.ExitStatus(), nil
	}
	return -1, fmt.Errorf("failed to get Return Code: %v", err)
}

func createContainerGroupYAMLFile(aciName string, aciLocation string, image string, env []environmentVariableDef) (string, error) {

	containerProperties := containerPropertiesDef{
		Image: image,
		Resources: containerResourcesDef{
			Requests: containerRequestsDef{
				Cpu:        1,
				MemoryInGb: "1.5",
			},
		},
		EnvironmentVariables: env,
	}

	containers := []containerDef{
		containerDef{
			Name:       aciName,
			Properties: containerProperties,
		},
	}

	containerGroup := containerGroupDef{
		Name:         aciName,
		Location:     aciLocation,
		ApiVersion:   "2018-10-01",
		ResourceType: "Microsoft.ContainerInstance",
		Properties: containerGroupPropertiesDef{
			Ostype:        "Linux",
			RestartPolicy: "Never",
			Containers:    containers,
		},
	}
	bytes, err := yaml.Marshal(containerGroup)
	if err != nil {
		return "", fmt.Errorf("failed to serialize config to YAML:, %v", err)
	}
	tmpfile, err := ioutil.TempFile("", "config*")
	if err != nil {
		return "", fmt.Errorf("failed to create temp config file:, %v", err)
	}

	if _, err := tmpfile.Write(bytes); err != nil {
		return "", fmt.Errorf("failed to write to temp config file:, %v", err)
	}
	if err := tmpfile.Close(); err != nil {
		return "", fmt.Errorf("failed to close temp config file:, %v", err)
	}

	return tmpfile.Name(), nil

}

type containerGroupDef struct {
	ApiVersion   string
	Location     string
	Name         string
	ResourceType string `yaml:"type"`
	Properties   containerGroupPropertiesDef
}

type containerGroupPropertiesDef struct {
	Ostype        string
	RestartPolicy string
	Containers    []containerDef
}

type containerDef struct {
	Name       string
	Properties containerPropertiesDef
}
type containerPropertiesDef struct {
	Image                string
	Resources            containerResourcesDef
	EnvironmentVariables []environmentVariableDef
}
type containerResourcesDef struct {
	Requests containerRequestsDef
}
type containerRequestsDef struct {
	Cpu        int
	MemoryInGb string
}
type environmentVariableDef struct {
	Name        string
	SecureValue string
}
