# Remote Trainer Runtime (HPC via FirecREST)

This repository implements a custom Kubeflow Trainer runtime that executes training jobs on an external HPC system instead of inside Kubernetes.

It is built on top of a fork of Kubeflow Trainer (based on the latest stable release v2.1.0).

The runtime is designed to be simple and low-overhead:
- Kubernetes handles orchestration
- Actual compute runs on HPC via FirecREST
- Minimal components, minimal complexity

## Overview

Unlike standard Kubeflow runtimes that execute workloads inside Kubernetes, this runtime:

1. Retrieves the training script:
   - either by extracting it from the Trainer SDK command
   - or by downloading it from S3 via a provided annotation
   - the SLURM submission script is also fetched from S3
2. Passes it to a lightweight runner container
3. The runner submits and monitors the job on HPC via FirecREST
4. Logs are fetched from the remote system
5. The TrainJob status reflects the HPC job outcome

## Repository Structure

- `remote.go`
  Runtime plugin implementation.
  Modifies the generated JobSet:
  - injects runner container
  - mounts training script
  - sets environment variables
  - disables retries via FailurePolicy

- `firecrest_runner.py`
  Executes the job on HPC:
  - downloads scripts from S3
  - uploads files to HPC
  - submits SLURM job via FirecREST
  - waits for completion
  - fetches logs
  - annotates TrainJob with job ID

- `Dockerfile`
  Builds the runner image

- `remote_test.go`
  Unit tests for plugin behavior

## Architecture

This runtime consists of two parts:

### 1. Runtime plugin

Built into a custom `trainer-controller-manager` image.

Responsible for:
- modifying JobSet templates
- injecting runner execution
- wiring configuration into the pod

### 2. Kubernetes runtime definition

The runtime is exposed via a `ClusterTrainingRuntime` resource:

```yaml
apiVersion: trainer.kubeflow.org/v1alpha1
kind: ClusterTrainingRuntime
metadata:
  name: remote
  labels:
    trainer.kubeflow.org/framework: remote
spec:
  mlPolicy:
    numNodes: 1
  template:
    spec:
      replicatedJobs:
        - name: node
          template:
            metadata:
              labels:
                trainer.kubeflow.org/trainjob-ancestor-step: trainer
            spec:
              template:
                metadata:
                  annotations:
                    sidecar.istio.io/inject: "false"
                spec:
                  restartPolicy: Never
                  containers:
                    - name: node
                      image: <your-runner-image>
                      command:
                        - python3
                        - /runner/firecrest_runner.py
                      imagePullPolicy: Always
                      envFrom:
                        - secretRef:
                            name: aws-secret
                        - secretRef:
                            name: firecrest-secret
```
This resource is typically deployed via Kubeflow overlays (e.g. Kustomize), together with:

- image overrides
- RBAC
- secrets
- networking policies

## Required Environment Variables
These are provided via Kubernetes Secrets, which are pointed to in remote.yaml example above:
### firecrest-secret
```
FIRECREST_URL
FIRECREST_TOKEN
FIRECREST_MACHINE
FIRECREST_REMOTE_PATH
FIRECREST_ACCOUNT
```
### aws-secret
```
AWS_ACCESS_KEY_ID
AWS_SECRET_ACCESS_KEY
S3_ENDPOINT_URL
S3_REGION
```

## Usage

The remote runtime is used like any other Trainer runtime.

The training script can be provided in two ways:

- **Inline**, as a Python function (via the Trainer SDK)
- **From S3**, by specifying a script URI using an annotation
    - Some dummy script must be given as func to Trainer SDK, for example:
    ```python
    def dummy():
        pass
    ````

In both cases, a SLURM submission script must be provided via S3 as an annotation.

### Example (Python client)

```python
from kubeflow.trainer import TrainerClient
from kubeflow.trainer.options import Annotations

job_id = TrainerClient().train(
    runtime="remote",
    trainer=CustomTrainer(
        func=dummy,
        num_nodes=1,
        resources_per_node={
            "cpu": 1,
            "memory": "1Gi",
        },
    ),
    options=[
        Annotations({
            "remote.trainer.kubeflow.org/slurm-uri": "s3://my-bucket/test-slurm.sh",
            "remote.trainer.kubeflow.org/script-uri": "s3://my-bucket/test_script.py"
        })
    ],
)
```
## Execution Flow
- User submits a TrainJob with runtime `remote`
- Plugin modifies JobSet:
    - injects runner
    - mounts script
- Runner starts inside the pod
- Runner:
    - downloads scripts from S3
    - uploads them to HPC
    - submits SLURM job via FirecREST
    - waits for completion
- Logs are fetched and printed
- TrainJob status is updated

## Failure Behavior
- JobSet retries are disabled (FailurePolicy: MaxRestarts=0)
- If the HPC job fails → TrainJob fails
    - No automatic retries → User must inspect logs and resubmit

## Notes

- If the Kubernetes pod terminates, the HPC job continues independently
- The system does not currently reattach to running HPC jobs
- The HPC job ID is stored as a TrainJob annotation for traceability

## Design Principles

- Keep the system simple
- Minimize moving parts
- Avoid unnecessary orchestration layers

## Status

This project is a work in progress.

The core functionality is implemented and has been used in test workflows, but the runtime is still evolving and may change.

Some features and edge cases (e.g. reconnecting to running jobs) are not yet implemented.
