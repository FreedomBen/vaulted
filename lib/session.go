package vaulted

import (
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go/aws/arn"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

var (
	SessionVersion = "2"
)

type Session struct {
	SessionVersion string `json:"version"`

	Name       string    `json:"name"`
	Expiration time.Time `json:"expiration"`

	ActiveRole string `json:"active_role,omitempty"`

	AWSCreds *AWSCredentials   `json:"aws_creds,omitempty"`
	Role     string            `json:"role,omitempty"`
	Vars     map[string]string `json:"vars,omitempty"`
	SSHKeys  map[string]string `json:"ssh_keys,omitempty"`
}

func (e *Session) AssumeSessionRole() (*Session, error) {
	if e.Role == "" {
		return e, nil
	}

	role := e.Role
	e.Role = ""

	return e.AssumeRole(role)
}

func (e *Session) AssumeRole(roleArn string) (*Session, error) {
	expiration := e.Expiration
	maxExpiration := time.Now().Add(time.Hour).Truncate(time.Second)
	if expiration.After(maxExpiration) {
		expiration = maxExpiration
	}

	duration := expiration.Sub(time.Now())

	var creds *AWSCredentials

	selectedRoleArn := roleArn
	parsedArn, err := arn.Parse(roleArn)
	if err == nil {
		creds, err = e.AWSCreds.AssumeRole(parsedArn.String(), duration)
		if err != nil {
			return nil, err
		}
	} else {
		// Unparseable ARN
		identityArn, err := e.AWSCreds.GetCallerIdentity()
		if err != nil {
			return nil, err
		}

		fullRoleArn := arn.ARN{
			Partition: identityArn.Partition,
			Service:   "iam",
			Region:    "",
			AccountID: identityArn.AccountID,
			Resource:  "role/" + roleArn,
		}

		selectedRoleArn = fullRoleArn.String()
		creds, err = e.AWSCreds.AssumeRole(selectedRoleArn, duration)
		if err != nil {
			return nil, fmt.Errorf("Error assuming role '%s' which was interpreted as '%s'\nError: %v", roleArn, selectedRoleArn, err)
		}
	}

	session := &Session{
		SessionVersion: SessionVersion,

		Name:       e.Name,
		Expiration: *creds.Expiration,

		ActiveRole: selectedRoleArn,

		AWSCreds: creds,
		Vars:     make(map[string]string),
		SSHKeys:  make(map[string]string),
	}
	for key, value := range e.Vars {
		session.Vars[key] = value
	}
	for key, value := range e.SSHKeys {
		session.SSHKeys[key] = value
	}

	return session, nil
}

func (e *Session) Spawn(cmd []string) (*int, error) {
	if len(cmd) == 0 {
		return nil, ErrInvalidCommand
	}

	// lookup the path of the executable
	cmdpath, err := exec.LookPath(cmd[0])
	if err != nil {
		return nil, fmt.Errorf("Cannot find executable %s: %v", cmd[0], err)
	}

	// start the agent
	sock, err := e.startProxyKeyring()
	if err != nil {
		return nil, err
	}

	vars := make(map[string]string)
	vars["SSH_AUTH_SOCK"] = sock

	// trap signals
	sigs := make(chan os.Signal)
	signal.Notify(
		sigs,
		syscall.SIGHUP,
		syscall.SIGINT,
		syscall.SIGQUIT,
		syscall.SIGTERM,
		syscall.SIGUSR1,
		syscall.SIGUSR2,
		syscall.SIGWINCH,
	)

	// start the process
	var attr os.ProcAttr
	attr.Env = e.buildEnviron(vars)
	attr.Files = []*os.File{os.Stdin, os.Stdout, os.Stderr}

	proc, err := os.StartProcess(cmdpath, cmd, &attr)
	if err != nil {
		return nil, fmt.Errorf("Failed to execute command: %v", err)
	}

	// relay trapped signals to the spawned process
	go func() {
		for s := range sigs {
			proc.Signal(s)
		}
	}()

	defer func() {
		signal.Stop(sigs)
		close(sigs)
	}()

	// wait for the process to exit
	state, _ := proc.Wait()

	var exitStatus int
	if !state.Success() {
		if status, ok := state.Sys().(syscall.WaitStatus); ok {
			exitStatus = status.ExitStatus()
		} else {
			exitStatus = 255
		}
	}

	// we only return an error if spawning the process failed, not if
	// the spawned command returned a failure status code.
	return &exitStatus, nil
}

func (e *Session) startProxyKeyring() (string, error) {
	keyring, err := NewProxyKeyring(os.Getenv("SSH_AUTH_SOCK"))
	if err != nil {
		return "", err
	}

	// load ssh keys
	for comment, key := range e.SSHKeys {
		timeRemaining := e.Expiration.Sub(time.Now())
		addedKey := agent.AddedKey{
			Comment:      comment,
			LifetimeSecs: uint32(timeRemaining.Seconds()),
		}

		addedKey.PrivateKey, err = ssh.ParseRawPrivateKey([]byte(key))
		if err != nil {
			return "", err
		}

		err := keyring.Add(addedKey)
		if err != nil {
			return "", err
		}
	}

	sock, err := keyring.Listen()
	if err != nil {
		return "", err
	}

	go keyring.Serve()

	return sock, err
}

func (e *Session) Variables() *Variables {
	vars := Variables{
		Set: make(map[string]string),
	}

	for key, value := range e.Vars {
		vars.Set[key] = value
	}

	vars.Set["VAULTED_ENV"] = e.Name
	vars.Set["VAULTED_ENV_EXPIRATION"] = e.Expiration.UTC().Format(time.RFC3339)

	if e.ActiveRole != "" {
		vars.Set["VAULTED_ENV_ROLE_ARN"] = e.ActiveRole

		roleArn, err := arn.Parse(e.ActiveRole)
		if err == nil {
			resource := strings.TrimPrefix(roleArn.Resource, "role/")
			vars.Set["VAULTED_ENV_ROLE_PARTITION"] = roleArn.Partition
			vars.Set["VAULTED_ENV_ROLE_ACCOUNT_ID"] = roleArn.AccountID
			vars.Set["VAULTED_ENV_ROLE_NAME"] = path.Base(resource)
			vars.Set["VAULTED_ENV_ROLE_PATH"] = fmt.Sprintf("/%s/", path.Dir(resource))
		}
	}

	if e.AWSCreds != nil {
		vars.Set["AWS_ACCESS_KEY_ID"] = e.AWSCreds.ID
		vars.Set["AWS_SECRET_ACCESS_KEY"] = e.AWSCreds.Secret

		if e.AWSCreds.Token != "" {
			vars.Set["AWS_SESSION_TOKEN"] = e.AWSCreds.Token
			vars.Set["AWS_SECURITY_TOKEN"] = e.AWSCreds.Token
		} else {
			vars.Unset = append(
				vars.Unset,
				"AWS_SESSION_TOKEN",
				"AWS_SECURITY_TOKEN",
			)
		}
	}

	return &vars
}

func (e *Session) buildEnviron(extraVars map[string]string) []string {
	vars := make(map[string]string)
	for _, v := range os.Environ() {
		parts := strings.SplitN(v, "=", 2)
		vars[parts[0]] = parts[1]
	}

	v := e.Variables()
	for _, key := range v.Unset {
		delete(vars, key)
	}
	for key, value := range v.Set {
		vars[key] = value
	}

	for key, value := range extraVars {
		vars[key] = value
	}

	environ := make([]string, 0, len(vars))
	for key, value := range vars {
		environ = append(environ, fmt.Sprintf("%s=%s", key, value))
	}
	return environ
}

type Variables struct {
	Set   map[string]string
	Unset []string
}
