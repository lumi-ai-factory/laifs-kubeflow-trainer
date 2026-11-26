# Remote Trainer Runtime (HPC via FirecREST)

A custom Kubeflow Trainer runtime that runs training jobs on an HPC system via FirecREST instead of
inside Kubernetes. It is built on a fork of Kubeflow Trainer v2.4.0.

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
  - adds the credential Secret named in the `credentials` annotation (`envFrom`)
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
                      # The credential Secret is added by the plugin
                      envFrom:
                        - configMapRef:
                            name: mlflow-client
                            optional: true
```

## Configuration

| Source | Variables |
|---|---|
| Secret named in the `credentials` annotation | `FIRECREST_URL`, `FIRECREST_MACHINE`, `FIRECREST_REMOTE_PATH`, `FIRECREST_ACCOUNT`, FirecREST auth (see below), `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `S3_ENDPOINT_URL`, `S3_REGION` |
| `mlflow-client` ConfigMap (optional) | `MLFLOW_TRACKING_URI` |

### Credentials

Each job names its credential Secret with the `credentials` annotation, and the plugin adds that Secret from the
TrainJob's namespace to the runner. The job runs on HPC as the owner of its FirecREST credentials. A credential Secret
belongs to one user (e.g. `lumi-jane-doe-example-org`) or to a robot account (`lumi-robot`). Only names
starting with `lumi-` are accepted. There is no default: without the annotation the TrainJob does not start.

The Secret holds one of two FirecREST auth methods:

| Method | Keys | Notes |
|---|---|---|
| Robot account (preferred) | `FIRECREST_CLIENT_ID`, `FIRECREST_CLIENT_SECRET` | Robot username and password. The runner gets a new token from the CSC IdP whenever the old one is about to expire. |
| Personal token | `FIRECREST_TOKEN` | 24 h token from the FirecREST API; must be replaced when it expires. |

If both are set, the robot account is used and `FIRECREST_TOKEN` is ignored with a warning. If only one of the robot
keys is set, or neither method is set, the runner exits with an error. On LUMI a robot account's default SLURM
account is `default_no_jobs`, so `FIRECREST_ACCOUNT` must name the project (e.g. `project_<number>`).

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
| `remote.trainer.kubeflow.org/credentials` | Credential Secret, `lumi-...` (required) |
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
            "remote.trainer.kubeflow.org/credentials": "lumi-jane-doe-example-org",
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
- Without the `slurm-uri` or `credentials` annotation, the TrainJob does not start (the plugin returns an error).
- If the credential Secret does not exist, the runner pod does not start (`CreateContainerConfigError`).
- Without the MLflow ConfigMap or token, the job runs without MLflow (warning in the runner log).

## Notes

- If the Kubernetes pod terminates, the HPC job continues independently.
- The runtime does not reattach to running HPC jobs.

## Status

Work in progress: the core functionality is used in test workflows, but the runtime may still change.
