package systemd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	log "github.com/sirupsen/logrus"

	"github.com/docker/go-units"
	enginetypes "github.com/projecteru2/core/engine/types"
	"github.com/projecteru2/core/utils"
)

const (
	unitTemplate = `
[Unit]
%s

[Service]
%s
	`
)

type unitBuilder struct {
	ID            string
	opts          *enginetypes.VirtualizationCreateOptions
	unitBuffer    []string
	serviceBuffer []string
	err           error
}

type unitDesciption struct {
	ID     string
	Name   string
	Labels map[string]string
}

func (s *SystemdSSH) newUnitBuilder(ID string, opts *enginetypes.VirtualizationCreateOptions) *unitBuilder {
	return &unitBuilder{
		ID:   ID,
		opts: opts,
	}
}

func (b *unitBuilder) cgroupPath() string {
	return b.ID
}

func (b *unitBuilder) buildUnit() *unitBuilder {
	if b.err != nil {
		return b
	}

	description, err := json.Marshal(unitDesciption{Name: b.opts.Name, Labels: b.opts.Labels})
	if err != nil {
		b.err = err
		return b
	}

	b.unitBuffer = append(b.unitBuffer, []string{
		fmt.Sprintf("Description=%s", string(description)),
		"After=network-online.target firewalld.service",
		"Wants=network-online.target",
	}...)
	return b
}

func (b *unitBuilder) buildPreExec() *unitBuilder {
	if b.err != nil {
		return b
	}

	b.serviceBuffer = append(b.serviceBuffer,
		fmt.Sprintf("ExecStartPre=/usr/bin/cgcreate -g memory,cpuset:%s", b.cgroupPath()),
	)

	return b.buildCPULimit().buildMemoryLimit()
}

func (b *unitBuilder) buildCPULimit() *unitBuilder {
	if b.err != nil {
		return b
	}

	if len(b.opts.CPU) > 0 {
		allowedCPUs := []string{}
		for CPU, _ := range b.opts.CPU {
			allowedCPUs = append(allowedCPUs, CPU)
		}
		b.serviceBuffer = append(b.serviceBuffer,
			fmt.Sprintf("ExecStartPre=/usr/bin/cgset -r cpuset.cpus=%s %s", strings.Join(allowedCPUs, ","), b.cgroupPath()),
		)
	}

	if b.opts.Quota > 0 {
		b.serviceBuffer = append(b.serviceBuffer,
			fmt.Sprintf("CPUQuota=%.2f%%", b.opts.Quota*100),
		)
	}

	numaNode := b.opts.NUMANode
	if numaNode == "" {
		numaNode = "0"
	}
	b.serviceBuffer = append(b.serviceBuffer,
		fmt.Sprintf("ExecStartPre=/usr/bin/cgset -r cpuset.mems=%s %s", numaNode, b.cgroupPath()),
	)

	return b
}
func (b *unitBuilder) buildMemoryLimit() *unitBuilder {
	if b.err != nil {
		return b
	}

	if b.opts.SoftLimit {
		b.serviceBuffer = append(b.serviceBuffer,
			fmt.Sprintf("ExecStartPre=/usr/bin/cgset -r memory.soft_limit_in_bytes=%d %s", b.opts.Memory, b.cgroupPath()),
		)

	} else {
		b.serviceBuffer = append(b.serviceBuffer,
			fmt.Sprintf("ExecStartPre=/usr/bin/cgset -r memory.limit_in_bytes=%d %s", b.opts.Memory, b.cgroupPath()),
			fmt.Sprintf("ExecStartPre=/usr/bin/cgset -r memory.soft_limit_in_bytes=%d %s", utils.Max(int(b.opts.Memory/2), units.MiB*4), b.cgroupPath()),
		)
	}
	return b
}

func (b *unitBuilder) buildExec() *unitBuilder {
	if b.err != nil {
		return b
	}

	env := []string{}
	for _, e := range b.opts.Env {
		env = append(env, fmt.Sprintf(`"%s"`, e))
	}

	stdioType, err := b.convertToSystemdStdio(b.opts.LogType)
	if err != nil {
		b.err = err
		return b
	}

	restartPolicy, err := b.convertToSystemdRestartPolicy(b.opts.RestartPolicy)
	if err != nil {
		b.err = err
		return b
	}

	b.serviceBuffer = append(b.serviceBuffer, []string{
		fmt.Sprintf("ExecStart=/usr/bin/cgexec -g memory,cpuset:%s %s", b.cgroupPath(), strings.Join(b.opts.Cmd, " ")),
		fmt.Sprintf("Environment=%s", strings.Join(env, " ")),
		fmt.Sprintf("StandardOutput=%s", stdioType),
		fmt.Sprintf("StandardError=%s", stdioType),
		fmt.Sprintf("Restart=%s", restartPolicy),
	}...)
	return b
}

func (b *unitBuilder) buildPostExec() *unitBuilder {
	if b.err != nil {
		return b
	}

	b.serviceBuffer = append(b.serviceBuffer,
		fmt.Sprintf("ExecStopPost=/usr/bin/cgdelete -g cpuset,memory:%s", b.cgroupPath()),
	)
	return b
}

func (b *unitBuilder) buffer() (*bytes.Buffer, error) {
	unit := fmt.Sprintf(unitTemplate,
		strings.Join(b.unitBuffer, "\n"),
		strings.Join(b.serviceBuffer, "\n"),
	)
	log.Debugf("%s", unit)
	return bytes.NewBufferString(unit), b.err
}

func (b *unitBuilder) convertToSystemdRestartPolicy(restart string) (policy string, err error) {
	switch {
	case restart == "no":
		policy = "no"
	case restart == "always" || restart == "":
		policy = "always"
	case strings.HasPrefix(restart, "on-failure"):
		policy = "on-failure"
	default:
		err = fmt.Errorf("restart policy not supported: %s", restart)
	}
	return
}

func (b *unitBuilder) convertToSystemdStdio(logType string) (stdioType string, err error) {
	switch logType {
	case "journald", "":
		stdioType = "journal"
	case "none":
		stdioType = "null"
	default:
		err = fmt.Errorf("log type not supported: %s", logType)
	}
	return
}