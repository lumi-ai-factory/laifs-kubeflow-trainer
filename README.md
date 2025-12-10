# Remote ClusterTrainingRuntime

**NOTE: Work in progress**

This repository extends **Kubeflow Trainer** with a custom `remote` **ClusterTrainingRuntime** that executes user-defined training code on an external machine via **SSH** instead of running inside the Kubernetes cluster.

The runtime takes the Python function passed to `CustomTrainer`, serializes it into a script, mounts it into the training container, and uses an SSH runner to upload and execute the script on a remote host.


## How It Works

1. User submits a `CustomTrainer(func=...)` job in a notebook.
2. Kubeflow Trainer SDK injects the code into the `TrainJob` spec.
3. The `remote` runtime plugin:
   - Stores the script into a ConfigMap
   - Mounts the script into the training container at `/app/script.py`
   - Adds required SSH-related environment variables
4. `ssh_runner.py` inside the container:
   - Reads `SCRIPT_PATH`
   - Connects to the remote host using SSH (private key from a Secret)
   - Uploads `/app/script.py` to the remote machine
   - Executes it with `python3`
   - Streams stdout/stderr back to the pod logs


## Components

| File            | Purpose                                           |
| --------------- | ------------------------------------------------- |
| `remote.go`     | The runtime plugin that injects script + env vars |
| `ssh_runner.py` | SSH executor that uploads and runs the script     |
| `Dockerfile`    | Builds the `ssh-trainer` image                    |
| `remote.yaml`   | Defines the ClusterTrainingRuntime                |


## Current state

***Works***

- remote code exec through Kubeflow TrainJob

- synchronous job execution

- capturing stdout from remote host

***Limitations***

- Not production ready

- Requires manual Minikube setup

- SSH host and user are hardcoded

- Requires local Docker build and image load

***Next steps:***

- Use Firecrest/HeaPPE instead of SSH

- Dynamically inject credentials instead of hardcoding them

- Package the runtime so it can be included as a clean overlay