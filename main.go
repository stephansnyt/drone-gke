package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/urfave/cli"
)

var (
	rev string
)

func main() {
	err := wrapMain()
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

func wrapMain() error {
	if rev == "" {
		rev = "[unknown]"
	}

	fmt.Printf("Drone GDM Plugin built from %s\n", rev)

	app := cli.NewApp()
	app.Name = "gdm plugin"
	app.Usage = "gdm plugin"
	app.Action = run
	app.Version = fmt.Sprintf("1.0.0-%s", rev)
	app.Flags = []cli.Flag{
		cli.BoolFlag{
			Name:   "preview",
			Usage:  "run gdm in preview mode",
			EnvVar: "PLUGIN_PREVIEW",
		},
		cli.BoolFlag{
			Name:   "verbose",
			Usage:  "verbose output including interpolated templates",
			EnvVar: "PLUGIN_VERBOSE",
		},
		cli.StringFlag{
			Name:   "token",
			Usage:  "service account's JSON credentials",
			EnvVar: "PLUGIN_TOKEN",
		},
		cli.StringFlag{
			Name:   "gcloud-cmd",
			Usage:  "alternative gcloud cmd",
			EnvVar: "PLUGIN_GCLOUD_CMD",
		},
		cli.StringFlag{
			Name:   "project",
			Usage:  "gcp project",
			EnvVar: "PLUGIN_PROJECT",
		},
		cli.StringFlag{
			Name:   "deploymentName",
			Usage:  "name of the deploymant",
			EnvVar: "PLUGIN_DEPLOYMENT_NAME",
		},
		cli.StringFlag{
			Name:   "template",
			Usage:  "template for deployment configuration",
			EnvVar: "PLUGIN_TEMPLATE",
			Value:  ".gdm.yaml",
		},
		cli.StringFlag{
			Name:   "vars",
			Usage:  "variables to use in template",
			EnvVar: "PLUGIN_VARS",
		},
		cli.StringFlag{
			Name:   "drone-build-number",
			Usage:  "variables to use in secret_template. These should already be base64 encoded; the plugin will not do so.",
			EnvVar: "DRONE_BUILD_NUMBER",
		},
		cli.StringFlag{
			Name:   "drone-commit",
			Usage:  "variables to use in secret_template. These should already be base64 encoded; the plugin will not do so.",
			EnvVar: "DRONE_COMMIT",
		},
		cli.StringFlag{
			Name:   "drone-branch",
			Usage:  "variables to use in secret_template. These should already be base64 encoded; the plugin will not do so.",
			EnvVar: "DRONE_BRANCH",
		},
		cli.StringFlag{
			Name:   "drone-tag",
			Usage:  "variables to use in secret_template. These should already be base64 encoded; the plugin will not do so.",
			EnvVar: "DRONE_TAG",
		},
	}

	if err := app.Run(os.Args); err != nil {
		return err
	}

	return nil
}

func run(c *cli.Context) error {
	vars := map[string]string
	if c.String("vars") != "" {
		if err := json.Unmarshal([]byte(c.String("vars")), &vargs.Vars); err != nil {
			panic(err)
		}
	}
	// Check required params.

	if vargs.Token == "" {
		return fmt.Errorf("Missing required param: token")
	}

	if vargs.Project == "" {
		vargs.Project = getProjectFromToken(vargs.Token)
	}

	if vargs.Project == "" {
		return fmt.Errorf("Missing required param: project")
	}

	if vargs.Zone == "" {
		return fmt.Errorf("Missing required param: zone")
	}

	sdkPath := "/google-cloud-sdk"
	keyPath := "/tmp/gcloud.json"

	// Defaults.

	if vargs.GCloudCmd == "" {
		vargs.GCloudCmd = fmt.Sprintf("%s/bin/gcloud", sdkPath)
	}

	if vargs.KubectlCmd == "" {
		vargs.KubectlCmd = fmt.Sprintf("%s/bin/kubectl", sdkPath)
	}

	if vargs.Template == "" {
		vargs.Template = ".kube.yml"
	}

	if vargs.SecretTemplate == "" {
		vargs.SecretTemplate = ".kube.sec.yml"
	}

	// Trim whitespace, to forgive the vagaries of YAML parsing.
	vargs.Token = strings.TrimSpace(vargs.Token)

	// Write credentials to tmp file to be picked up by the 'gcloud' command.
	// This is inside the ephemeral plugin container, not on the host.
	err := ioutil.WriteFile(keyPath, []byte(vargs.Token), 0600)
	if err != nil {
		return fmt.Errorf("Error writing token file: %s\n", err)
	}

	// Warn if the keyfile can't be deleted, but don't abort.
	// We're almost certainly running inside an ephemeral container, so the file will be discarded when we're finished anyway.
	defer func() {
		err := os.Remove(keyPath)
		if err != nil {
			fmt.Printf("Warning: error removing token file: %s\n", err)
		}
	}()

	e := os.Environ()
	e = append(e, fmt.Sprintf("GOOGLE_APPLICATION_CREDENTIALS=%s", keyPath))
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("Error while getting working directory: %s\n", err)
	}
	runner := NewEnviron(wd, e, os.Stdout, os.Stderr)

	err = runner.Run(vargs.GCloudCmd, "auth", "activate-service-account", "--key-file", keyPath)
	if err != nil {
		return fmt.Errorf("Error: %s\n", err)
	}

	err = runner.Run(vargs.GCloudCmd, "container", "clusters", "get-credentials", vargs.Cluster, "--project", vargs.Project, "--zone", vargs.Zone)
	if err != nil {
		return fmt.Errorf("Error: %s\n", err)
	}

	data := map[string]interface{}{
		"BUILD_NUMBER": build.Number,
		"COMMIT":       build.Commit,
		"BRANCH":       build.Branch,
		"TAG":          build.Tag,

		// https://godoc.org/github.com/drone/drone-plugin-go/plugin#Workspace
		// TODO do we really need these?
		// "workspace": workspace,
		// "repo":      repo,
		// "build":     build,
		// "system":    system,

		// Misc useful stuff.
		// Note that we don't include all of the vargs, since that includes the GCP token.
		"project":   vargs.Project,
		"zone":      vargs.Zone,
		"cluster":   vargs.Cluster,
		"namespace": vargs.Namespace,
	}

	for k, v := range vargs.Vars {
		// Don't allow vars to be overridden.
		// We do this to ensure that the built-in template vars (above) can be relied upon.
		if _, ok := data[k]; ok {
			return fmt.Errorf("Error: var %q shadows existing var\n", k)
		}

		data[k] = v
	}

	if vargs.Verbose {
		dump := data
		delete(dump, "workspace")
		dumpData(os.Stdout, "DATA (Workspace Values Omitted)", dump)
	}

	secrets := map[string]interface{}{}
	for k, v := range vargs.Secrets {
		if v == "" {
			return fmt.Errorf("Error: secret var %q is an empty string\n", k)
		}

		// Base64 encode secret strings.
		secrets[k] = base64.StdEncoding.EncodeToString([]byte(v))
	}
	for k, v := range vargs.SecretsBase64 {
		if _, ok := secrets[k]; ok {
			return fmt.Errorf("Error: secret var %q is already set in Secrets\n", k)
		}
		if v == "" {
			return fmt.Errorf("Error: secret var %q is an empty string\n", k)
		}
		// Don't base64 encode these secrets, they already are.
		secrets[k] = v
	}

	mapping := map[string]map[string]interface{}{
		vargs.Template:       data,
		vargs.SecretTemplate: secrets,
	}

	outPaths := make(map[string]string)
	pathArg := []string{}

	for t, content := range mapping {
		if t == "" {
			continue
		}

		inPath := filepath.Join(wd, t)
		bn := filepath.Base(inPath)

		// Ensure the required template file exists.
		_, err := os.Stat(inPath)
		if os.IsNotExist(err) {
			if t == vargs.Template {
				return fmt.Errorf("Error finding template: %s\n", err)
			} else {
				fmt.Printf("Warning: skipping optional template %s, it was not found\n", t)
				continue
			}
		}

		// Generate the file.
		blob, err := ioutil.ReadFile(inPath)
		if err != nil {
			return fmt.Errorf("Error reading template: %s\n", err)
		}

		tmpl, err := template.New(bn).Option("missingkey=error").Parse(string(blob))
		if err != nil {
			return fmt.Errorf("Error parsing template: %s\n", err)
		}

		outPaths[t] = fmt.Sprintf("/tmp/%s", bn)
		f, err := os.Create(outPaths[t])
		if err != nil {
			return fmt.Errorf("Error creating deployment file: %s\n", err)
		}

		err = tmpl.Execute(f, content)
		if err != nil {
			return fmt.Errorf("Error executing deployment template: %s\n", err)
		}

		f.Close()

		pathArg = append(pathArg, outPaths[t])
	}

	if vargs.Verbose {
		dumpFile(os.Stdout, "DEPLOYMENT (Secret Template Omitted)", outPaths[vargs.Template])
	}

	if vargs.DryRun {
		fmt.Println("Skipping kubectl apply, because dry_run: true")
		return nil
	}

	// Set the execution namespace.
	if len(vargs.Namespace) > 0 {
		fmt.Printf("Configuring kubectl to the %s namespace\n", vargs.Namespace)

		context := strings.Join([]string{"gke", vargs.Project, vargs.Zone, vargs.Cluster}, "_")

		err = runner.Run(vargs.KubectlCmd, "config", "set-context", context, "--namespace", vargs.Namespace)
		if err != nil {
			return fmt.Errorf("Error: %s\n", err)
		}

		resource := fmt.Sprintf(nsTemplate, vargs.Namespace)
		nsPath := "/tmp/namespace.json"

		// Write namespace resource file to tmp file to be picked up by the 'kubectl' command.
		// This is inside the ephemeral plugin container, not on the host.
		err := ioutil.WriteFile(nsPath, []byte(resource), 0600)
		if err != nil {
			return fmt.Errorf("Error writing namespace resource file: %s\n", err)
		}

		// Ensure the namespace exists, without errors (unlike `kubectl create namespace`).
		err = runner.Run(vargs.KubectlCmd, "apply", "--filename", nsPath)
		if err != nil {
			return fmt.Errorf("Error: %s\n", err)
		}
	}

	// Apply Kubernetes configuration files.
	err = runner.Run(vargs.KubectlCmd, "apply", "--filename", strings.Join(pathArg, ","))
	if err != nil {
		return fmt.Errorf("Error: %s\n", err)
	}

	return nil
}

type token struct {
	ProjectID string `json:"project_id"`
}

func getProjectFromToken(j string) string {
	t := token{}
	err := json.Unmarshal([]byte(j), &t)
	if err != nil {
		return ""
	}
	return t.ProjectID
}
