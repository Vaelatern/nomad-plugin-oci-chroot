package driver

import (
	"github.com/hashicorp/nomad/plugins/shared/hclspec"
)

type TaskConfig struct {
	Image       string   `codec:"image"`
	Command     string   `codec:"command"`
	Args        []string `codec:"args"`
	WorkDir     string   `codec:"work_dir"`
	BindSockets []string `codec:"bind_sockets"`
	ForcePull   bool     `codec:"force_pull"`
}

var taskConfigSpec = hclspec.NewObject(map[string]*hclspec.Spec{
	"image":        hclspec.NewAttr("image", "string", true),
	"command":      hclspec.NewAttr("command", "string", false),
	"args":         hclspec.NewAttr("args", "list(string)", false),
	"work_dir":     hclspec.NewAttr("work_dir", "string", false),
	"bind_sockets": hclspec.NewAttr("bind_sockets", "list(string)", false),
	"force_pull":   hclspec.NewAttr("force_pull", "bool", false),
})

type OciChrootTaskState struct {
	ContainerName string `codec:"container_name"`
	MountPoint    string `codec:"mount_point"`
	PID           int    `codec:"pid"`
	ImageRef      string `codec:"image_ref"`
	NetnsPath     string `codec:"netns_path"`
}
