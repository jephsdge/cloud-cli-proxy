package http

import (
	"os"
	"strings"
)

const defaultWorkerUser = "work"

func configuredWorkerUser() string {
	if user := strings.TrimSpace(os.Getenv("CLOUD_CLI_PROXY_WORKER_USER")); user != "" {
		return user
	}
	return defaultWorkerUser
}

func configuredWorkerHome() string {
	if home := strings.TrimSpace(os.Getenv("CLOUD_CLI_PROXY_WORKER_HOME")); home != "" {
		return home
	}
	return "/home/" + configuredWorkerUser()
}

func workerStatePath(name string) string {
	return strings.TrimRight(configuredWorkerHome(), "/") + "/" + strings.TrimLeft(name, "/")
}

func workerSSHDir() string {
	return workerStatePath(".ssh")
}
