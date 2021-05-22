/*
MIT License

Copyright (c) Nhost

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/
package cmd

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v2"
)

type (

	// Container service
	Container struct {
		Image       string                 `yaml:",omitempty"`
		Name        string                 "container_name"
		Command     []string               `yaml:",omitempty"`
		Entrypoint  string                 `yaml:",omitempty"`
		Environment map[string]interface{} `yaml:",omitempty"`
		Ports       []string               `yaml:",omitempty"`
		Restart     string                 `yaml:",omitempty"`
		User        string                 `yaml:",omitempty"`
		Volumes     []string               `yaml:",omitempty"`
		DependsOn   []string               `yaml:"depends_on,omitempty"`
		EnvFile     []string               `yaml:"env_file,omitempty"`
		Build       map[string]string      `yaml:",omitempty"`
	}

	// Container services
	Services struct {
		Containers map[string]Container `yaml:"services,omitempty"`
		Version    string               `yaml:",omitempty"`
	}
)

// devCmd represents the dev command
var devCmd = &cobra.Command{
	Use:   "dev",
	Short: "Start local development environment",
	Long:  `Initialize a local Nhost environment for development and testing.`,
	Run: func(cmd *cobra.Command, args []string) {

		Print("Initializing dev environment", "info")

		// check if /nhost exists
		if !pathExists(nhostDir) {
			Error(nil, "project not found in this directory\nto initialize a project, run 'nhost' or 'nhost init'", true)
		}

		// check if /.nhost exists
		if !pathExists(dotNhost) {
			if err := os.MkdirAll(dotNhost, os.ModePerm); err != nil {
				Error(err, "couldn't initialize nhost specific directory", true)
			}
		}

		// connect to docker client
		ctx := context.Background()
		docker, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			Error(err, "failed to connect to docker client", true)
		}

		// check if this is the first time dev env is running
		firstRun := !pathExists(path.Join(dotNhost, "db_data"))
		if firstRun {
			Print("first run takes longer, please be patient", "warn")

			// if it doesn't exist, then create it
			if err = os.MkdirAll(path.Join(dotNhost, "db_data"), os.ModePerm); err != nil {
				Error(err, "failed to create db_data directory", true)
			}
		}

		// add cleanup action in case of signal interruption
		c := make(chan os.Signal)
		signal.Notify(c, os.Interrupt, syscall.SIGTERM)
		go func() {
			<-c
			cleanup(docker, ctx, dotNhost, "interrupted by signal")
			os.Exit(1)
		}()

		/*

			// read pre-written docker-compose yaml
			preWrittenConfigFile, _ := ioutil.ReadFile(path.Join(dotNhost, "docker-compose.yaml"))
			var preWrittenConfig Services
			yaml.Unmarshal(preWrittenConfigFile, &preWrittenConfig)

			// start containers from Nhost Backend Yaml
			for _, service := range preWrittenConfig.Containers {
				for serviceName, _ := range nhostServices {
					fmt.Println(serviceName, " : ", service[serviceName])
				}
			}
		*/

		nhostConfig, err := readYaml(path.Join(nhostDir, "config.yaml"))
		if err != nil {
			Error(err, "couldn't read Nhost config", true)
		}

		ports := []string{
			"hasura_graphql_port",
			"hasura_backend_plus_port",
			"postgres_port",
			"minio_port",
			"api_port",
		}

		var mappedPorts []string

		for _, port := range ports {
			mappedPorts = append(mappedPorts, fmt.Sprintf("%v", nhostConfig[port]))
		}

		mappedPorts = append(mappedPorts, "9695")

		freePorts := getFreePorts(mappedPorts)

		var occupiedPorts []string
		for _, port := range mappedPorts {
			if !contains(freePorts, port) {
				occupiedPorts = append(occupiedPorts, port)
			}
		}

		if len(occupiedPorts) > 0 {

			Error(
				nil,
				fmt.Sprintf("ports %v are already in use, hence aborting\nchange nhost/config.yaml or stop the services", occupiedPorts),
				true,
			)
		}

		// generate Nhost service containers' configurations
		nhostServices, err := getContainerConfigs(docker, ctx, nhostConfig, dotNhost)
		if err != nil {
			Error(err, "", false)
			cleanup(docker, ctx, dotNhost, "failed to generate container configurations")
		}

		for _, container := range nhostServices {
			if err = runContainer(docker, ctx, container); err != nil {
				Error(err, fmt.Sprintf("failed to start %v container", container.ID), true)
				cleanup(docker, ctx, dotNhost, "failed to start an Nhost service")
			}
			Print(fmt.Sprintf("container %s created", container.ID), "success")
		}

		nhostConfig["startAPI"] = pathExists(path.Join(workingDir, "api"))
		nhostConfig["graphql_jwt_key"] = generateRandomKey()

		// write docker api file
		_, err = os.Create(path.Join(dotNhost, "Dockerfile-api"))
		if err != nil {
			Error(err, "failed to create docker api config", false)
		}

		err = writeToFile(path.Join(dotNhost, "Dockerfile-api"), getDockerApiTemplate(), "start")
		if err != nil {
			Error(err, "failed to write backend docker-compose config", true)
		}

		/*
			// skip the use of docker-compose since docker sdk is being used

			nhostBackendYaml, _ := generateNhostBackendYaml(nhostConfig)

			// create docker-compose.yaml
			nhostBackendYamlFilePath := path.Join(dotNhost, "docker-compose.yaml")
			_, err = os.Create(nhostBackendYamlFilePath)
			if err != nil {
				Error(err, "failed to create docker-compose config", false)
			}

			// write nhost backend configuration to docker-compose.yaml to auth file
			config, _ := yaml.Marshal(nhostBackendYaml)

			err = writeToFile(nhostBackendYamlFilePath, string(config), "end")
			if err != nil {
				Error(err, "failed to write backend docker-compose config", true)
			}

				// get docker-compose path
				dockerComposeCLI, _ := exec.LookPath("docker-compose")

				// validate compose file
				execute := exec.Cmd{
					Path: dockerComposeCLI,
					Args: []string{dockerComposeCLI, "-f", nhostBackendYamlFilePath, "config"},
				}

				output, err := execute.CombinedOutput()
				if err != nil {
					Error(err, "failed to validate docker-compose config", false)
					cleanup(docker, ctx, dotNhost, string(output))
				}

				// run docker-compose up
				execute = exec.Cmd{
					Path: dockerComposeCLI,
					Args: []string{dockerComposeCLI, "-f", "nhostBackendYamlFilePath", "up", "-d", "--build"},
				}

				output, err = execute.CombinedOutput()
				if err != nil {
					Error(err, "failed to start docker-compose", false)
					cleanup(docker, ctx, dotNhost, string(output))
				}
		*/

		Print("waiting for GraphQL engine to go up", "info")
		// check whether GraphQL engine is up & running
		if !validateGraphqlEngineRunning(nhostConfig["hasura_graphql_port"]) {
			cleanup(docker, ctx, dotNhost, "failed to start GraphQL Engine")
		}

		// prepare and load hasura binary
		hasuraCLI, _ := loadBinary("hasura", hasura)

		commandOptions := []string{
			"--endpoint",
			fmt.Sprintf(`http://localhost:%v`, nhostConfig["hasura_graphql_port"]),
			"--admin-secret",
			fmt.Sprintf(`%v`, nhostConfig["hasura_graphql_admin_secret"]),
			"--skip-update-check",
		}

		if VERBOSE {
			Print("applying migrations", "info")
		}

		// create migrations from remote
		cmdArgs := []string{hasuraCLI, "migrate", "apply"}
		cmdArgs = append(cmdArgs, commandOptions...)

		execute := exec.Cmd{
			Path: hasuraCLI,
			Args: cmdArgs,
			Dir:  nhostDir,
		}

		output, err := execute.CombinedOutput()
		if err != nil {
			Error(errors.New(string(output)), "", false)
			os.Exit(1)
			cleanup(docker, ctx, dotNhost, "failed to apply fresh hasura migrations")
		}

		files, err := ioutil.ReadDir(path.Join(nhostDir, "seeds"))
		if err != nil {
			Error(errors.New(string(output)), "", false)
			cleanup(docker, ctx, dotNhost, "failed to read migrations directory")
		}

		if firstRun && len(files) > 0 {

			if VERBOSE {
				Print("applying seeds", "info")
			}

			// apply seed data
			cmdArgs = []string{hasuraCLI, "seeds", "apply"}
			cmdArgs = append(cmdArgs, commandOptions...)

			execute = exec.Cmd{
				Path: hasuraCLI,
				Args: cmdArgs,
				Dir:  nhostDir,
			}

			output, err = execute.CombinedOutput()
			if err != nil {
				Error(err, "failed to apply seed data", false)
				cleanup(docker, ctx, dotNhost, string(output))
			}
		}

		if VERBOSE {
			Print("applying metadata", "info")
		}

		// create migrations from remote
		cmdArgs = []string{hasuraCLI, "metadata", "apply"}
		cmdArgs = append(cmdArgs, commandOptions...)

		execute = exec.Cmd{
			Path: hasuraCLI,
			Args: cmdArgs,
			Dir:  nhostDir,
		}

		output, err = execute.CombinedOutput()
		if err != nil {
			Error(err, "failed to aapply fresh metadata", false)
			cleanup(docker, ctx, dotNhost, string(output))
		}

		/*

			switch runtime.GOOS {
			case "linux":
				err = exec.Command("xdg-open", url).Start()
			case "windows":
				err = exec.Command("rundll32", "url.dll,FileProtocolHandler", url).Start()
			case "darwin":
				err = exec.Command("open", url).Start()
			default:
				err = fmt.Errorf("unsupported platform")
			}
			if err != nil {
				log.Fatal(err)
			}
		*/

		Print("Local Nhost backend is up!\n", "success")

		if containerRunning(docker, ctx, "nhost_hasura") {
			Print(fmt.Sprintf("GraphQL API: http://localhost:%v/v1/graphql", nhostConfig["hasura_graphql_port"]), "info")
		}
		if containerRunning(docker, ctx, "nhost_hbp") {
			Print(fmt.Sprintf("Auth & Storage: http://localhost:%v", nhostConfig["hasura_backend_plus_port"]), "info")
		}
		if containerRunning(docker, ctx, "nhost_api") {
			Print(fmt.Sprintf("Custom API: http://localhost:%v\n", nhostConfig["api_port"]), "info")
		}

		Print("launching Hasura console", "info")
		//spawn hasura console in parallel terminal session
		hasuraConsoleSpawnCmd := exec.Cmd{
			Path: hasuraCLI,
			Args: []string{hasuraCLI,
				"console",
				"--endpoint",
				fmt.Sprintf(`http://localhost:%v`, nhostConfig["hasura_graphql_port"]),
				"--admin-secret",
				fmt.Sprintf(`%v`, nhostConfig["hasura_graphql_admin_secret"]),
				"--console-port",
				"9695",
			},
			Dir: nhostDir,
		}

		if err = hasuraConsoleSpawnCmd.Run(); err != nil {
			Error(err, "failed to launch hasura console", false)
		} else {
			Print("Hasura Console: `http://localhost:9695`", "info")
		}

		Print("Press Ctrl + C to stop running evironment", "waiting")

		// wait for user input infinitely to keep the utility running
		scanner := bufio.NewScanner(os.Stdin)
		scanner.Scan()
	},
}

// check if a container is running by specified name
func containerRunning(cli *client.Client, ctx context.Context, name string) bool {

	containers, _ := cli.ContainerList(ctx, types.ContainerListOptions{All: true})
	for _, container := range containers {
		if strings.Contains(container.Names[0], name) {
			return true
		}
	}

	return false
}

// start a fresh container in background
func runContainer(client *client.Client, ctx context.Context, cont container.ContainerCreateCreatedBody) error {

	err := client.ContainerStart(ctx, cont.ID, types.ContainerStartOptions{})

	/*
		// avoid using the code below if you want to run the containers in background

		statusCh, errCh := client.ContainerWait(ctx, cont.ID, container.WaitConditionNotRunning)
		select {
		case err := <-errCh:
			if err != nil {
				return err
			}
		case <-statusCh:
		}
	*/
	return err
}

// run cleanup
func cleanup(client *client.Client, ctx context.Context, location, errorMessage string) {

	Error(errors.New(errorMessage), "cleanup/rollback process initiated", false)

	logFile := path.Join(location, "nhost.log")

	// stop all running containers with prefix "nhost_"
	if err := shutdownServices(client, ctx, logFile); err != nil {
		Error(err, "failed to shutdown running Nhost services", false)
	}

	// kill any running hasura console on port 9695
	hasuraConsoleKillCmd := exec.Command("fuser", "-k", "9695/tcp")
	if err := hasuraConsoleKillCmd.Run(); err != nil {
		Error(err, "failed to kill hasura console session", false)
	}

	deletePath(path.Join(location, "Dockerfile-api"))

	Print("cleanup complete", "info")
	Print("See you later, grasshopper!", "success")
	os.Exit(0)
}

func validateGraphqlEngineRunning(port interface{}) bool {

	for i := 1; i <= 10; i++ {
		valid := graphqlEngineHealthCheck(port)
		if valid {
			if VERBOSE {
				Print(fmt.Sprintf("GraphQL engine health check attempt #%v successful", i), "success")
			}
			return true
		}
		time.Sleep(2 * time.Second)
		if VERBOSE {
			Print(fmt.Sprintf("GraphQL engine health check attempt #%v unsuccessful", i), "warn")
		}
	}

	if VERBOSE {
		Error(nil, "GraphQL engine health check timed out", false)
	}
	return false
}

func graphqlEngineHealthCheck(port interface{}) bool {

	client := http.Client{
		Timeout: 10 * time.Second,
	}

	resp, err := client.Get(fmt.Sprintf(`http://localhost:%v/healthz`, port))
	if err != nil {
		return false
	}

	defer resp.Body.Close()

	body, _ := ioutil.ReadAll(resp.Body)
	if string(body) == "OK" {
		return true
	}
	return false
}

// generate a random 128 byte key
func generateRandomKey() string {
	key := make([]byte, 128)
	rand.Read(key)
	return hex.EncodeToString(key)
}

// check whether source array contains value or not
func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}

// read YAML files
func readYaml(path string) (map[string]interface{}, error) {

	f, err := ioutil.ReadFile(path)

	var data map[string]interface{}
	yaml.Unmarshal(f, &data)

	return data, err
}

func getFreePorts(ports []string) []string {

	var freePorts []string

	for _, port := range ports {
		if portAvaiable(port) {
			freePorts = append(freePorts, port)
		}
	}
	return freePorts
}

func portAvaiable(port string) bool {

	ln, err := net.Listen("tcp", ":"+port)

	if err != nil {
		return false
	}

	ln.Close()
	return true
}

func getDockerApiTemplate() string {
	return `
FROM nhost/nodeapi:v0.2.7
WORKDIR /usr/src/app
COPY api ./api
RUN ./install.sh
ENTRYPOINT ["./entrypoint-dev.sh"]
`
}

func getContainerConfigs(client *client.Client, ctx context.Context, options map[string]interface{}, cwd string) ([]container.ContainerCreateCreatedBody, error) {

	hasuraGraphQLEngine := "hasura/graphql-engine"

	if options["hasura_graphql_engine"] != nil && options["hasura_graphql_engine"] != "" {
		hasuraGraphQLEngine = fmt.Sprintf(`%v`, options["hasura_graphql_engine"])
	}
	var containers []container.ContainerCreateCreatedBody

	// check if a required already exists
	// if it doesn't which case -> then pull it

	requiredImages := []string{
		fmt.Sprintf("postgres:%v", options["postgres_version"]),
		fmt.Sprintf("%s:%v", hasuraGraphQLEngine, options["hasura_graphql_version"]),
		fmt.Sprintf("nhost/hasura-backend-plus:%v", options["hasura_backend_plus_version"]),
		"minio/minio:latest",
	}

	availableImages, err := getInstalledImages(client, ctx)
	if err != nil {
		return containers, err
	}

	for _, requiredImage := range requiredImages {
		// check wether the image is available or not
		available := false
		for _, image := range availableImages {
			// if it NOT available, then pull the image
			if contains(image.RepoTags, requiredImage) {
				available = true
			}
		}

		if !available {
			if err = pullImage(client, ctx, requiredImage); err != nil {
				Error(err, fmt.Sprintf("failed to pull image %s\nplease pull it manually and re-run %snhost dev%s", requiredImage, Bold, Reset), false)
			}
		}
	}

	// read env_file
	envFile, err := ioutil.ReadFile(envFile)
	if err != nil {
		Print(fmt.Sprintf("failed to read %v file", options["env_file"]), "warn")
		return containers, err
	}

	envData := strings.Split(string(envFile), "\n")
	var envVars []string

	for _, row := range envData {
		if strings.Contains(row, "=") {
			envVars = append(envVars, row)
		}
	}

	/*
		// Define Network config (why isn't PORT in here...?:
		// https://godoc.org/github.com/docker/docker/api/types/network#NetworkingConfig
		networkConfig := &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{},
		}
			gatewayConfig := &network.EndpointSettings{
				Gateway: "gatewayname",
			}
			networkConfig.EndpointsConfig["bridge"] = gatewayConfig
	*/

	postgresContainer, err := client.ContainerCreate(
		ctx,
		&container.Config{
			Image: fmt.Sprintf(`postgres:%v`, options["postgres_version"]),
			Env: []string{
				fmt.Sprintf("POSTGRES_USER=%v", options["postgres_user"]),
				fmt.Sprintf("POSTGRES_PASSWORD=%v", options["postgres_password"]),
			},
			ExposedPorts: nat.PortSet{nat.Port("5432"): struct{}{}},
		},
		&container.HostConfig{
			PortBindings: map[nat.Port][]nat.PortBinding{nat.Port("5432"): {{HostIP: "127.0.0.1", HostPort: "5432"}}},
			RestartPolicy: container.RestartPolicy{
				Name: "always",
			},
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeBind,
					Source: path.Join(cwd, "db_data"),
					Target: "/var/lib/postgresql/data",
				},
			},
		},
		nil,
		nil,
		"nhost_postgres",
	)

	if err != nil {
		return containers, err
	}

	// prepare env variables for following container
	containerVariables := []string{
		fmt.Sprintf("HASURA_GRAPHQL_SERVER_PORT=%v", options["hasura_graphql_port"]),
		fmt.Sprintf("HASURA_GRAPHQL_DATABASE_URL=%v", fmt.Sprintf(`postgres://%v:%v@nhost-postgres:5432/postgres`, options["postgres_user"], options["postgres_password"])),
		"HASURA_GRAPHQL_ENABLE_CONSOLE=false",
		"HASURA_GRAPHQL_ENABLED_LOG_TYPES=startup, http-log, webhook-log, websocket-log, query-log",
		fmt.Sprintf("HASURA_GRAPHQL_ADMIN_SECRET=%v", options["hasura_graphql_admin_secret"]),
		fmt.Sprintf("HASURA_GRAPHQL_MIGRATIONS_SERVER_TIMEOUT=%d", 20),
		fmt.Sprintf("HASURA_GRAPHQL_NO_OF_RETRIES=%d", 20),
		"HASURA_GRAPHQL_UNAUTHORIZED_ROLE=public",
		fmt.Sprintf("NHOST_HASURA_URL=%v", fmt.Sprintf(`http://nhost_hasura:%v/v1/graphql`, options["hasura_graphql_port"])),
		"NHOST_WEBHOOK_SECRET=devnhostwebhooksecret",
		fmt.Sprintf("NHOST_HBP_URL=%v", fmt.Sprintf(`http://nhost_hbp:%v`, options["hasura_backend_plus_port"])),
		fmt.Sprintf("NHOST_CUSTOM_API_URL=%v", fmt.Sprintf(`http://nhost_api:%v`, options["api_port"])),
	}
	containerVariables = append(containerVariables, envVars...)

	// if user has saved Hasura JWT Key, add that as well
	if options["graphql_jwt_key"] != nil {
		containerVariables = append(containerVariables,
			fmt.Sprintf("HASURA_GRAPHQL_JWT_SECRET=%v", fmt.Sprintf(`{"type":"HS256", "key": "%v"}`, options["graphql_jwt_key"])))
	}

	graphqlEngineContainer, err := client.ContainerCreate(
		ctx,
		&container.Config{
			Image: fmt.Sprintf(`%s:%v`, hasuraGraphQLEngine, options["hasura_graphql_version"]),
			Env:   containerVariables,
			ExposedPorts: nat.PortSet{
				nat.Port(strconv.Itoa(options["hasura_graphql_port"].(int))): struct{}{},
			},
			//Cmd:          []string{"graphql-engine", "serve"},
		},
		&container.HostConfig{
			Links: []string{"nhost_postgres:nhost-postgres"},
			PortBindings: map[nat.Port][]nat.PortBinding{
				nat.Port(strconv.Itoa(options["hasura_graphql_port"].(int))): {{HostIP: "127.0.0.1",
					HostPort: strconv.Itoa(options["hasura_graphql_port"].(int))}},
			},
			RestartPolicy: container.RestartPolicy{
				Name: "always",
			},
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeBind,
					Source: path.Join(cwd, "db_data"),
					Target: "/var/lib/postgresql/data",
				},
			},
		},
		nil,
		nil,
		"nhost_hasura",
	)

	if err != nil {
		return containers, err
	}

	// create mount point if it doesn't exit
	customMountPoint := path.Join(dotNhost, "minio", "data")
	if !pathExists(customMountPoint) {
		if err = os.MkdirAll(customMountPoint, os.ModePerm); err != nil {
			Error(err, "failed to create .nhost/minio/data directory", false)
		}
	}

	// repeat for .nhost/minio/config dir
	customMountPoint = path.Join(dotNhost, "minio", "config")
	if !pathExists(customMountPoint) {
		if err = os.MkdirAll(customMountPoint, os.ModePerm); err != nil {
			Error(err, "failed to create .nhost/minio/config directory", false)
		}
	}

	minioContainer, err := client.ContainerCreate(
		ctx,
		&container.Config{
			Image: `minio/minio`,
			//User:  "999:1001",
			Env: []string{
				"MINIO_ACCESS_KEY=minioaccesskey123123",
				"MINIO_SECRET_KEY=minioaccesskey123123",
				//"MINIO_ROOT_USER=AKIAIOSFODNN7EXAMPLE",
				//"MINIO_ROOT_PASSWORD=wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
			},
			ExposedPorts: nat.PortSet{nat.Port(strconv.Itoa(options["minio_port"].(int))): struct{}{}},
			Entrypoint:   []string{"sh"},
			Cmd: []string{
				"-c",
				fmt.Sprintf(`/usr/bin/minio server --address :%v /data`, options["minio_port"]),
			},
		},
		&container.HostConfig{
			PortBindings: map[nat.Port][]nat.PortBinding{
				nat.Port(strconv.Itoa(options["minio_port"].(int))): {{HostIP: "127.0.0.1",
					HostPort: strconv.Itoa(options["minio_port"].(int))}}},
			RestartPolicy: container.RestartPolicy{
				Name: "always",
			},
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeBind,
					Source: path.Join(cwd, "minio", "data"),
					Target: "/data",
				},
				{
					Type:   mount.TypeBind,
					Source: path.Join(cwd, "minio", "config"),
					Target: "/.minio",
				},
			},
		},
		nil,
		nil,
		"nhost_minio",
	)

	if err != nil {
		return containers, err
	}

	// run an extra minio "mc" container to add "nhost" bucket
	mcContainer, err := client.ContainerCreate(
		ctx,
		&container.Config{
			Image:        `minio/mc`,
			ExposedPorts: nat.PortSet{nat.Port(strconv.Itoa(options["minio_port"].(int))): struct{}{}},
			Entrypoint:   []string{"sh"},
			Cmd: []string{
				"-c",
				"/usr/bin/mc config host add myminio http://nhost-minio:9000 minioaccesskey123123 minioaccesskey123123;",
				"/usr/bin/mc rm -r --force myminio/nhost;",
				"/usr/bin/mc mb myminio/nhost;",
				"/usr/bin/mc policy download myminio/nhost;",
				"exit 0;",
			},
		},
		&container.HostConfig{
			Links: []string{"nhost_minio:nhost-minio"},
			RestartPolicy: container.RestartPolicy{
				Name: "always",
			},
		},
		nil,
		nil,
		"nhost_mc",
	)

	if err != nil {
		return containers, err
	}

	// prepare env variables for following container
	containerVariables = []string{
		fmt.Sprintf("PORT=%v", options["hasura_backend_plus_port"]),
		"USER_FIELDS=''",
		"USER_REGISTRATION_AUTO_ACTIVE=true",
		fmt.Sprintf("HASURA_GRAPHQL_ENDPOINT=%v", fmt.Sprintf(`http://nhost-graphql-engine:%v/v1/graphql`, options["hasura_graphql_port"])),
		fmt.Sprintf("HASURA_ENDPOINT=%v", fmt.Sprintf(`http://nhost-graphql-engine:%v/v1/graphql`, options["hasura_graphql_port"])),
		fmt.Sprintf("HASURA_GRAPHQL_ADMIN_SECRET=%v", options["hasura_graphql_admin_secret"]),
		"AUTH_ACTIVE=true",
		"AUTH_LOCAL_ACTIVE=true",
		"REFRESH_TOKEN_EXPIRES=43200",
		fmt.Sprintf("S3_ENDPOINT=%v", fmt.Sprintf(`nhost-minio:%v`, options["minio_port"])),
		"S3_SSL_ENABLED=false",
		"S3_BUCKET=nhost",
		"S3_ACCESS_KEY_ID=minioaccesskey123123",
		"S3_SECRET_ACCESS_KEY=miniosecretkey123123",
		"LOST_PASSWORD_ENABLE=true",
		fmt.Sprintf("PROVIDER_SUCCESS_REDIRECT=%v", options["provider_success_redirect"]),
		fmt.Sprintf("PROVIDER_FAILURE_REDIRECT=%v", options["provider_failure_redirect"]),
	}
	containerVariables = append(containerVariables, envVars...)

	// if user has saved Hasura JWT Key, add that as well
	if options["graphql_jwt_key"] != nil {
		containerVariables = append(containerVariables,
			fmt.Sprintf("JWT_KEY=%v", options["graphql_jwt_key"]),
		)
		containerVariables = append(containerVariables,
			"JWT_ALGORITHM=HS256",
		)
		containerVariables = append(containerVariables,
			"JWT_TOKEN_EXPIRES=15",
		)
	}

	// prepare social auth credentials for hasura backend plus container
	socialAuthPlatforms := []string{"GOOGLE", "FACEBOOK", "GITHUB", "LINKEDIN"}

	var credentials []string
	for _, value := range socialAuthPlatforms {
		dominations := []string{"ENABLE", "CLIENT_ID", "CLIENT_SECRET"}
		for _, variable := range dominations {
			credentials = append(credentials, fmt.Sprintf("%s_%s", value, variable))
		}
	}

	for _, credential := range credentials {
		if options[strings.ToLower(credential)] != nil {
			containerVariables = append(containerVariables, fmt.Sprintf("%s=%v", credential, options[strings.ToLower(credential)]))
		}
	}

	// create mount point if it doesn't exit
	customMountPoint = path.Join(nhostDir, "custom")
	if !pathExists(customMountPoint) {

		// if it doesn't exist, then create it
		if err = os.MkdirAll(customMountPoint, os.ModePerm); err != nil {
			Error(err, "failed to create /nhost/custom directory", false)
		}
	}

	hasuraBackendPlusContainer, err := client.ContainerCreate(
		ctx,
		&container.Config{
			Image:        fmt.Sprintf(`nhost/hasura-backend-plus:%v`, options["hasura_backend_plus_version"]),
			Env:          containerVariables,
			ExposedPorts: nat.PortSet{nat.Port(strconv.Itoa(options["hasura_backend_plus_port"].(int))): struct{}{}},
			//Cmd:          []string{"graphql-engine", "serve"},
		},
		&container.HostConfig{
			Links: []string{"nhost_hasura:nhost-graphql-engine", "nhost_minio:nhost-minio"},
			PortBindings: map[nat.Port][]nat.PortBinding{
				nat.Port(strconv.Itoa(options["hasura_backend_plus_port"].(int))): {{HostIP: "127.0.0.1",
					HostPort: strconv.Itoa(options["hasura_backend_plus_port"].(int))}}},
			RestartPolicy: container.RestartPolicy{
				Name: "always",
			},
			Mounts: []mount.Mount{
				{
					Type:   mount.TypeBind,
					Source: path.Join(nhostDir, "custom"),
					Target: "/app/custom",
				},
			},
		},
		nil,
		nil,
		"nhost_hbp",
	)

	if err != nil {
		return containers, err
	}

	if options["startAPI"] != nil && options["startAPI"].(bool) {

		// prepare env variables for following container
		containerVariables = []string{
			fmt.Sprintf("PORT=%v", options["api_port"]),
			fmt.Sprintf("NHOST_HASURA_URL=%v", fmt.Sprintf(`http://nhost-hasura:%v/v1/graphql`, options["hasura_graphql_port"])),
			fmt.Sprintf("NHOST_HASURA_ADMIN_SECRET=%v", options["hasura_graphql_admin_secret"]),
			"NHOST_WEBHOOK_SECRET=devnhostwebhooksecret",
			fmt.Sprintf("NHOST_HBP_URL=%v", fmt.Sprintf(`http://nhost-hbp:%v`, options["hasura_backend_plus_port"])),
			fmt.Sprintf("NHOST_CUSTOM_API_URL=%v", fmt.Sprintf(`http://nhost-api:%v`, options["api_port"])),
		}
		containerVariables = append(containerVariables, envVars...)

		// if user has saved Hasura JWT Key, add that as well
		if options["graphql_jwt_key"] != nil {
			containerVariables = append(containerVariables,
				fmt.Sprintf("NHOST_JWT_KEY=%v", options["graphql_jwt_key"]),
			)
			containerVariables = append(containerVariables,
				"NHOST_JWT_ALGORITHM=HS256",
			)
		}

		// create mount point if it doesn't exit
		customMountPoint = path.Join("api")
		if !pathExists(customMountPoint) {

			// if it doesn't exist, then create it
			if err = os.MkdirAll(customMountPoint, os.ModePerm); err != nil {
				Error(err, "failed to create /nhost/custom directory", false)
			}
		}

		APIContainer, err := client.ContainerCreate(
			ctx,
			&container.Config{
				Env:          containerVariables,
				ExposedPorts: nat.PortSet{nat.Port(strconv.Itoa(options["api_port"].(int))): struct{}{}},
				OnBuild: []string{
					"context:../",
					"dockerfile:./.nhost/Dockerfile-api",
				},
			},
			&container.HostConfig{
				Links: []string{"nhost_hasura:nhost-hasura", "nhost_hbp:nhost-hbp", "nhost_minio:nhost-minio"},
				PortBindings: map[nat.Port][]nat.PortBinding{
					nat.Port(strconv.Itoa(options["api_port"].(int))): {{HostIP: "127.0.0.1",
						HostPort: strconv.Itoa(options["api_port"].(int))}}},
				RestartPolicy: container.RestartPolicy{
					Name: "always",
				},
				Mounts: []mount.Mount{
					{
						Type:   mount.TypeBind,
						Source: path.Join("api"),
						Target: "/usr/src/app/api",
					},
				},
			},
			nil,
			nil,
			"nhost_api",
		)

		if err != nil {
			return containers, err
		}

		containers = append(containers, APIContainer)
	}

	containers = append(containers, postgresContainer)
	containers = append(containers, minioContainer)
	containers = append(containers, mcContainer)

	// add depends_on for following containers
	containers = append(containers, graphqlEngineContainer)
	containers = append(containers, hasuraBackendPlusContainer)
	return containers, err
}

func getInstalledImages(cli *client.Client, ctx context.Context) ([]types.ImageSummary, error) {
	if VERBOSE {
		Print("fetching available container images", "info")
	}
	images, err := cli.ImageList(ctx, types.ImageListOptions{All: true})
	return images, err
}

func pullImage(cli *client.Client, ctx context.Context, tag string) error {
	if VERBOSE {
		Print(fmt.Sprintf("pulling container image: %s%s%s", Bold, tag, Reset), "info")
	}
	out, err := cli.ImagePull(ctx, tag, types.ImagePullOptions{})
	out.Close()
	return err
}

/*

// Legacy docker-compose generation

func generateNhostBackendYaml(options map[string]interface{}) (Services, error) {

	hasuraGraphQLEngine := "hasura/graphql-engine"

	if options["hasura_graphql_engine"] != nil {
		hasuraGraphQLEngine = options["hasura_graphql_engine"].(string)
	}

	postgresContainer := Container{
		Name:    "nhost_postgres",
		Image:   fmt.Sprintf(`postgres:%v`, options["postgres_version"]),
		Ports:   []string{fmt.Sprintf(`%v:5432`, options["postgres_port"])},
		Restart: "always",
		Environment: map[string]interface{}{
			"POSTGRES_USER":     "postgres_user",
			"POSTGRES_PASSWORD": "postgres_password",
		},
		// not sure whether this volume would work on windows as well
		Volumes: []string{"./db_data:/var/lib/postgresql/data"},
	}

	graphqlEngineContainer := Container{
		Name:      "nhost_hasura",
		Image:     fmt.Sprintf(`%s:%v`, hasuraGraphQLEngine, options["hasura_graphql_version"]),
		Ports:     []string{fmt.Sprintf(`%v:%v`, options["hasura_graphql_port"], options["hasura_graphql_port"])},
		Restart:   "always",
		DependsOn: []string{"nhost-postgres"},
		Environment: map[string]interface{}{
			"HASURA_GRAPHQL_SERVER_PORT":               options["hasura_graphql_port"],
			"HASURA_GRAPHQL_DATABASE_URL":              fmt.Sprintf(`"postgres://%v:%v@nhost-postgres:5432/postgres"`, options["postgres_user"], options["postgres_password"]),
			"HASURA_GRAPHQL_ENABLE_CONSOLE":            "false",
			"HASURA_GRAPHQL_ENABLED_LOG_TYPES":         "startup, http-log, webhook-log, websocket-log, query-log",
			"HASURA_GRAPHQL_ADMIN_SECRET":              options["hasura_graphql_admin_secret"],
			"HASURA_GRAPHQL_JWT_SECRET":                fmt.Sprintf(`{"type":"HS256", "key": "%v"}`, options["graphql_jwt_key"]),
			"HASURA_GRAPHQL_MIGRATIONS_SERVER_TIMEOUT": 20,
			"HASURA_GRAPHQL_NO_OF_RETRIES":             20,
			"HASURA_GRAPHQL_UNAUTHORIZED_ROLE":         "public",
			"NHOST_HASURA_URL":                         fmt.Sprintf("'http://nhost_hasura:%v/v1/graphql'", options["hasura_graphql_port"]),
			"NHOST_WEBHOOK_SECRET":                     "devnhostwebhooksecret",
			"NHOST_HBP_URL":                            fmt.Sprintf(`"http://nhost_hbp:%v"`, options["hasura_backend_plus_port"]),
			"NHOST_CUSTOM_API_URL":                     fmt.Sprintf(`"http://nhost_api:%v"`, options["api_port"]),
		},

		EnvFile: []string{options["env_file"].(string)},
		Command: []string{"graphql-engine", "serve"},
		// not sure whether this volume would work on windows as well
		Volumes: []string{"./db_data:/var/lib/postgresql/data"},
	}

	hasuraBackendPlusContainer := Container{
		Name:      "nhost_hbp",
		Image:     fmt.Sprintf(`nhost/hasura-backend-plus:%v`, options["hasura_backend_plus_version"]),
		Ports:     []string{fmt.Sprintf(`%v:%v`, options["hasura_backend_plus_port"], options["hasura_backend_plus_port"])},
		Restart:   "always",
		DependsOn: []string{"nhost-graphql-engine"},
		Environment: map[string]interface{}{
			"PORT":                          options["hasura_backend_plus_port"],
			"USER_FIELDS":                   "",
			"USER_REGISTRATION_AUTO_ACTIVE": "true",
			"HASURA_GRAPHQL_ENDPOINT":       fmt.Sprintf(`"http://nhost-graphql-engine:%v/v1/graphql"`, options["hasura_graphql_port"]),
			"HASURA_ENDPOINT":               fmt.Sprintf(`"http://nhost-graphql-engine:%v/v1/graphql"`, options["hasura_graphql_port"]),
			"HASURA_GRAPHQL_ADMIN_SECRET":   options["hasura_graphql_admin_secret"],
			"JWT_ALGORITHM":                 "HS256",
			"JWT_KEY":                       options["graphql_jwt_key"],
			"AUTH_ACTIVE":                   "true",
			"AUTH_LOCAL_ACTIVE":             "true",
			"REFRESH_TOKEN_EXPIRES":         43200,
			"JWT_TOKEN_EXPIRES":             15,
			"S3_ENDPOINT":                   fmt.Sprintf(`"nhost_minio:%v"`, options["minio_port"]),
			"S3_SSL_ENABLED":                "false",
			"S3_BUCKET":                     "nhost",
			"S3_ACCESS_KEY_ID":              "minioaccesskey123123",
			"S3_SECRET_ACCESS_KEY":          "miniosecretkey123123",
			"LOST_PASSWORD_ENABLE":          "true",
			"PROVIDER_SUCCESS_REDIRECT":     options["provider_success_redirect"],
			"PROVIDER_FAILURE_REDIRECT":     options["provider_failure_redirect"],
		},

		EnvFile: []string{options["env_file"].(string)},
		Command: []string{"graphql-engine", "serve"},

		// not sure whether this volume would work on windows as well
		Volumes: []string{fmt.Sprintf("%s:/app/custom", path.Join(nhostDir, "custom"))},
	}

	// add social auth credentials
	socialAuthPlatforms := []string{"GOOGLE", "FACEBOOK", "GITHUB", "LINKEDIN"}

	var credentials []string
	for _, value := range socialAuthPlatforms {
		dominations := []string{"ENABLE", "CLIENT_ID", "CLIENT_SECRET"}
		for _, variable := range dominations {
			credentials = append(credentials, fmt.Sprintf("%s_%s", value, variable))
		}
	}

	for _, credential := range credentials {
		if options[strings.ToLower(credential)] != nil {
			hasuraBackendPlusContainer.Environment[credential] = options[strings.ToLower(credential)]
		}
	}

	minioContainer := Container{
		Name:    "nhost_minio",
		Image:   `minio/minio`,
		User:    `999:1001`,
		Ports:   []string{fmt.Sprintf(`%v:%v`, options["minio_port"], options["minio_port"])},
		Restart: "always",
		Environment: map[string]interface{}{
			"MINIO_ACCESS_KEY": "minioaccesskey123123",
			"MINIO_SECRET_KEY": "minioaccesskey123123",
		},
		Entrypoint: "sh",
		Command:    []string{fmt.Sprintf(`"-c 'mkdir -p /data/nhost && /usr/bin/minio server --address :%v /data'"`, options["minio_port"])},

		// not sure whether this volume would work on windows as well
		Volumes: []string{`./minio/data:/data`, `./minio/config:/.minio`},
	}

	services := Services{
		Containers: map[string]Container{
			"nhost-postgres":            postgresContainer,
			"nhost-graphql-engine":      graphqlEngineContainer,
			"nhost-hasura-backend-plus": hasuraBackendPlusContainer,
			"minio":                     minioContainer,
		},
	}

	project := Services{
		Version:    "3.6",
		Containers: services.Containers,
	}

	if options["startAPI"].(bool) {

		APIContainer := Container{
			Name: "nhost_api",

			// not sure whether the following build command would work in windows or not
			Build: map[string]string{
				"context":    "../",
				"dockerfile": "./.nhost/Dockerfile-api",
			},

			Ports:   []string{fmt.Sprintf(`%v:%v`, options["api_port"], options["api_port"])},
			Restart: "always",
			Environment: map[string]interface{}{
				"PORT":                      options["api_port"],
				"NHOST_JWT_ALGORITHM":       "HS256",
				"NHOST_JWT_KEY":             options["graphql_jwt_key"],
				"NHOST_HASURA_URL":          fmt.Sprintf(`"http://nhost_hasura:%v/v1/graphql"`, options["hasura_graphql_port"]),
				"NHOST_HASURA_ADMIN_SECRET": options["hasura_graphql_admin_secret"],
				"NHOST_WEBHOOK_SECRET":      "devnhostwebhooksecret",
				"NHOST_HBP_URL":             fmt.Sprintf(`"http://nhost_hbp:%v"`, options["hasura_backend_plus_port"]),
				"NHOST_CUSTOM_API_URL":      fmt.Sprintf(`"http://nhost_api:%v"`, options["api_port"]),
			},
			EnvFile: []string{options["env_file"].(string)},

			// not sure whether this volume would work on windows as well
			Volumes: []string{"../api:/usr/src/app/api"},
		}
		services.Containers["nhost-api"] = APIContainer
	}

	return project, nil
}
*/

func init() {
	rootCmd.AddCommand(devCmd)

	// Here you will define your flags and configuration settings.

	// Cobra supports Persistent Flags which will work for this command
	// and all subcommands, e.g.:
	// devCmd.PersistentFlags().String("foo", "", "A help for foo")

	// Cobra supports local flags which will only run when this command
	// is called directly, e.g.:
	// devCmd.Flags().BoolP("toggle", "t", false, "Help message for toggle")
}