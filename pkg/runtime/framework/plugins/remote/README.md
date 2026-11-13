# Remote Trainer Runtime (HPC via FirecREST)

A custom Kubeflow Trainer runtime that runs training jobs on an HPC system via FirecREST instead of
inside Kubernetes. It is built on a fork of Kubeflow Trainer v2.2.0.

Developed by LUMI AI Factory and used to run training jobs from Kubeflow on LUMI.

The runtime is designed to be simple:
- Kubernetes handles orchestration
- Actual compute runs on HPC via FirecREST
- Minimal components, minimal complexity

## Repository Structure

- `remote.go`: runtime plugin, built into the custom `trainer-controller-manager` image. It modifies the
  generated JobSet:
  - stores the inline training script in a ConfigMap and mounts it, or passes the S3 script URI
  - passes the annotations and the TrainJob name as environment variables
  - runs the runner instead of the training command
  - disables retries (FailurePolicy `MaxRestarts=0`)
- `firecrest_runner.py`: the runner. It submits and follows the job on HPC (see Execution Flow).
- `Dockerfile`: builds the runner image.
- `remote_test.go`: unit tests for the plugin.

## Runtime Definition

The runtime is exposed as a `ClusterTrainingRuntime`, deployed with the Kubeflow setup:

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
                  serviceAccountName: default-editor
                  containers:
                    - name: node
                      image: <your-runner-image>
                      command:
                        - python3
                        - /runner/firecrest_runner.py
                      imagePullPolicy: Always
                      envFrom:
                        - secretRef:
                            name: s3-secret
                        - secretRef:
                            name: firecrest-secret
                        - configMapRef:
                            name: mlflow-client
                            optional: true
```

## Configuration

Provided by the Secrets and the ConfigMap referenced above:

| Source | Variables |
|---|---|
| `firecrest-secret` | `FIRECREST_URL`, `FIRECREST_TOKEN`, `FIRECREST_MACHINE`, `FIRECREST_REMOTE_PATH`, `FIRECREST_ACCOUNT` |
| `s3-secret` | `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `S3_ENDPOINT_URL`, `S3_REGION` |
| `mlflow-client` ConfigMap (optional) | `MLFLOW_TRACKING_URI` |

### MLflow (optional)

If the namespace has the `mlflow-client` ConfigMap, the runner requests a 48 h token for the `mlflow-client`
ServiceAccount and passes `MLFLOW_TRACKING_URI` and `MLFLOW_TRACKING_TOKEN` to the job. The runner's
ServiceAccount must be allowed to create these tokens.

## Usage

The training script is given either inline as a Python function (Trainer SDK) or from S3 with the
`script-uri` annotation. The SLURM script always comes from S3.

| Annotation | |
|---|---|
| `remote.trainer.kubeflow.org/slurm-uri` | S3 URI of the SLURM script (required) |
| `remote.trainer.kubeflow.org/script-uri` | S3 URI of the training script (optional, overrides the inline function) |
| `remote.trainer.kubeflow.org/job-id` | Set by the runner: the SLURM job ID |

```python
from kubeflow.trainer import CustomTrainer, TrainerClient
from kubeflow.trainer.options import Annotations

client = TrainerClient()
job_id = client.train(
    runtime=client.get_runtime("remote"),
    trainer=CustomTrainer(
        func=lambda *args, **kwargs: None,  # required by the SDK, ignored when script-uri is given
        num_nodes=1,
        resources_per_node={"cpu": 1, "memory": "1Gi"},
    ),
    options=[
        Annotations({
            "remote.trainer.kubeflow.org/slurm-uri": "s3://my-bucket/slurm_script.sh",
            "remote.trainer.kubeflow.org/script-uri": "s3://my-bucket/train.py",
        })
    ],
)
```

### SLURM script

- Run `source .env` to get the S3 and MLflow variables.
- Write the output to `firecrest-%j.out` and `firecrest-%j.err` (`#SBATCH --output`/`--error`).

## Execution Flow

1. The plugin modifies the JobSet and the runner pod starts.
2. The runner downloads the SLURM script (and the training script, if `script-uri` is given) from S3.
3. It requests the MLflow token and writes the S3 and MLflow variables to `.env`.
4. It uploads the scripts and `.env` (readable by its owner only) to
   `<FIRECREST_REMOTE_PATH>/kf_output/<date>/<time>_<id>/` on HPC.
5. It submits the SLURM job, sets the `job-id` annotation and waits for completion.
6. If the job completed, it prints the job's output; otherwise the TrainJob fails.

## Failure Behavior

- No retries: if the HPC job fails, the TrainJob fails, and the user must inspect the logs and resubmit.
- Without the `slurm-uri` annotation, the TrainJob does not start (the plugin returns an error).
- Without the MLflow ConfigMap or token, the job runs without MLflow (warning in the runner log).

## Notes

- If the Kubernetes pod terminates, the HPC job continues independently.
- The runtime does not reattach to running HPC jobs.

## Status

Work in progress: the core functionality is used in test workflows, but the runtime may still change.
