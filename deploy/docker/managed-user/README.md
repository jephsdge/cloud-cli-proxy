# 受管用户镜像说明

该镜像是“一用户一主机”模型下的标准模板容器，提供默认 SSH 工作环境、基础 Shell 工具以及预装的 `claude code`。

默认运行时约定如下：

- 主目录持久化挂载点默认为 `/home/work`
- 项目工作区固定为 `/workspace`，不再作为用户 HOME
- SSH 服务端 host key 按 host 持久化到宿主机 `runtime-data/hosts/<host_id>/ssh-host-keys`，容器内挂载为 `/etc/ssh/host-keys`
- 默认用户固定为 `work`
- 默认 UID/GID 为 `1000:1000`
- 控制面与 host-agent 必须统一读取 `image.lock` 中的同一个镜像全名
- 默认重建模式是 `preserve-home`，即重建系统层但保留 `/home/work`
- `factory_reset_mode: wipe-home` 仅作为后续显式工厂重置入口的契约，不在 Phase 1 自动执行
- 普通重建和工厂重置都不删除 SSH host key；只有删除 host 数据目录或重新创建新 host_id 时才会生成新的服务端身份

Phase 2 只允许在这个模板旁边新增网络准备钩子接口，不在本计划内落地隧道、出口 IP 绑定或其他网络强约束实现。

## 容器内运维脚本

- `restart-vnc`：重启 KasmVNC + 桌面进程（不重建容器）。
- `claude`：受管 `claude code` 二进制，直接安装在 `/usr/local/bin/claude`，不再额外包装中转层。
- `ll`：等价于 `ls -alF --color=auto`，同时在交互式 shell 中提供 `ll`/`la`/`l` alias。

## Worker 用户、Home、UID/GID

worker 容器运行时支持通过 control-plane 环境变量配置 Linux 用户身份：

- `CLOUD_CLI_PROXY_WORKER_USER`：容器内用户名，默认 `work`
- `CLOUD_CLI_PROXY_WORKER_UID`：容器内 UID，默认 `1000`
- `CLOUD_CLI_PROXY_WORKER_GID`：容器内 GID，默认 `1000`
- `CLOUD_CLI_PROXY_WORKER_HOME`：容器内主目录和持久化挂载点，可选；默认 `/home/${CLOUD_CLI_PROXY_WORKER_USER}`

如果把宿主机目录 bind mount 到 worker，建议把 UID/GID 配成宿主机用户一致，避免容器内初始化把目录属主改成 `1000:1000`。例如宿主机用户是 `work`：

```bash
CLOUD_CLI_PROXY_WORKER_USER=work
CLOUD_CLI_PROXY_WORKER_UID=$(id -u)
CLOUD_CLI_PROXY_WORKER_GID=$(id -g)
# CLOUD_CLI_PROXY_WORKER_HOME=/home/work  # 可选，默认会按用户名推导
```

这些变量由 control-plane 在创建新 worker 容器时透传为 `CONTAINER_USER`、`CONTAINER_UID`、`CONTAINER_GID`、`CONTAINER_HOME`。修改后需要重建 control-plane 并重新创建 worker 容器；已经存在的 worker 不会自动改名或迁移主目录。

SSH proxy 默认也会读取 `CLOUD_CLI_PROXY_WORKER_USER` 作为容器登录用户；如需单独覆盖，可设置 `SSH_PROXY_CONTAINER_USER`。

## 构建期自定义系统依赖

worker 镜像固定内置通用基础依赖，例如 `python3`、`python3-venv`、`python3-pip`。场景相关的大依赖通过构建参数注入，避免所有 worker 都被迫携带 PostgreSQL 等重组件。

在 `.env` 中配置：

```bash
CLOUD_CLI_PROXY_WORKER_EXTRA_APT_PACKAGES=postgresql postgresql-contrib postgresql-16-pgvector
```

然后重建 `managed-user-image`。构建结果会把实际额外包列表写入：

```text
/etc/cloud-claude/worker-extra-apt-packages
```

可以用下面命令验证：

```bash
docker run --rm ghcr.io/zanel1u/cloud-cli-proxy/managed-user:latest \
  cat /etc/cloud-claude/worker-extra-apt-packages
```
