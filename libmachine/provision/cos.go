package provision

import (
	"bytes"
	"fmt"
	"text/template"

	"github.com/docker/machine/libmachine/auth"
	"github.com/docker/machine/libmachine/drivers"
	"github.com/docker/machine/libmachine/engine"
	"github.com/docker/machine/libmachine/log"
	"github.com/docker/machine/libmachine/mcnutils"
	"github.com/docker/machine/libmachine/provision/pkgaction"
	"github.com/docker/machine/libmachine/provision/serviceaction"
	"github.com/docker/machine/libmachine/swarm"
	"github.com/docker/machine/libmachine/versioncmp"
)

func init() {
	Register("ContainerOptimizedOS", &RegisteredProvisioner{
		New: NewCOSProvisioner,
	})
}

func NewCOSProvisioner(d drivers.Driver) Provisioner {
	return &COSProvisioner{
		NewSystemdProvisioner("cos", d),
	}
}

type COSProvisioner struct {
	SystemdProvisioner
}

func (c *COSProvisioner) String() string {
	return "cos"
}

func (c *COSProvisioner) Package(_ string, _ pkgaction.PackageAction) error {
	return nil
}

func (c *COSProvisioner) GenerateDockerOptions(dockerPort int) (*DockerOptions, error) {
	var (
		engineCfg bytes.Buffer
	)

	driverNameLabel := fmt.Sprintf("provider=%s", c.Driver.DriverName())
	c.EngineOptions.Labels = append(c.EngineOptions.Labels, driverNameLabel)

	dockerVersion, err := DockerClientVersion(c)
	if err != nil {
		return nil, err
	}

	arg := "dockerd"
	if versioncmp.LessThan(dockerVersion, "1.12.0") {
		arg = "docker daemon"
	}

	engineConfigTmpl := `[Service]
ExecStart=
ExecStart=/usr/bin/` + arg + ` -H tcp://0.0.0.0:{{.DockerPort}} -H unix:///var/run/docker.sock --tlsverify --tlscacert {{.AuthOptions.CaCertRemotePath}} --tlscert {{.AuthOptions.ServerCertRemotePath}} --tlskey {{.AuthOptions.ServerKeyRemotePath}} {{ range .EngineOptions.Labels }}--label {{.}} {{ end }}{{ range .EngineOptions.InsecureRegistry }}--insecure-registry {{.}} {{ end }}{{ range .EngineOptions.RegistryMirror }}--registry-mirror {{.}} {{ end }}{{ range .EngineOptions.ArbitraryFlags }}--{{.}} {{ end }}
Environment={{range .EngineOptions.Env}}{{ printf "%q" . }} {{end}}
`
	t, err := template.New("engineConfig").Parse(engineConfigTmpl)
	if err != nil {
		return nil, err
	}

	engineConfigContext := EngineConfigContext{
		DockerPort:    dockerPort,
		AuthOptions:   c.AuthOptions,
		EngineOptions: c.EngineOptions,
	}

	t.Execute(&engineCfg, engineConfigContext)

	return &DockerOptions{
		EngineOptions:     engineCfg.String(),
		EngineOptionsPath: c.DaemonOptionsFile,
	}, nil
}

func (c *COSProvisioner) Provision(swarmOptions swarm.Options, authOptions auth.Options, engineOptions engine.Options) error {
	c.SwarmOptions = swarmOptions
	c.AuthOptions = authOptions
	c.EngineOptions = engineOptions
	swarmOptions.Env = engineOptions.Env

	log.Debug("setting hostname")
	err := c.SetHostname(c.Driver.GetMachineName())
	if err != nil {
		return err
	}

	log.Debug("waiting for docker daemon")
	err = mcnutils.WaitFor(c.dockerDaemonResponding)
	if err != nil {
		return err
	}

	c.AuthOptions = setRemoteAuthOptions(c)

	log.Debug("configuring auth")
	err = ConfigureAuth(c)
	if err != nil {
		return err
	}

	log.Debug("configure local firewall")
	err = c.configureLocalFirewall()
	if err != nil {
		return err
	}

	log.Debug("enabling docker in systemd")
	return c.Service("docker", serviceaction.Enable)
}

func (c *COSProvisioner) dockerDaemonResponding() bool {
	log.Debug("checking docker daemon")

	out, err := c.SSHCommand("sudo docker version")
	if err != nil {
		log.Warnf("Error getting SSH command to check if the daemon is up: %s", err)
		log.Debugf("'sudo docker version' output:\n%s", out)
		return false
	}

	return true
}

func (c *COSProvisioner) configureLocalFirewall() error {
	out, err := c.SSHCommand("sudo iptables -A INPUT -p tcp -m tcp --dport 2376 -j ACCEPT")
	if err != nil {
		log.Debugf("iptables output:\n%s", out)
		return err
	}

	return nil
}
