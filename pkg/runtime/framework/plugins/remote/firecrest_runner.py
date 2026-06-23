
#!/usr/bin/env python3
"""
FirecREST runner.

Executes a user-provided Python script on a remote SLURM cluster:
- Downloads SLURM submission script from S3 (SLURM_URI)
- Uploads user script + SLURM script to remote_path
- Submits job via FirecREST
- Waits for completion and prints remote logs

Fails fast on missing configuration.
"""
import os
import sys
from datetime import datetime
import uuid
import tempfile
from pathlib import Path

import firecrest as fc
from firecrest import FirecrestException
import boto3

class StaticTokenAuth:
    def __init__(self, token: str):
        self._token = token

    def get_access_token(self) -> str:
        return self._token

def require_env(name: str) -> str:
    """Fetch required environment variable or fail fast with a clear message."""
    v = os.environ.get(name)
    if not v:
        print(f"ERROR: missing env var '{name}'", file=sys.stderr)
        sys.exit(1)
    return v.strip()

def collect_firecrest_env():
    keys = [
        "FIRECREST_URL",
        "FIRECREST_TOKEN",
        "FIRECREST_MACHINE",
        "FIRECREST_REMOTE_PATH",
        "FIRECREST_ACCOUNT",
        ]
    env_vars = {}
    for key in keys:
        value = os.environ.get(key)
        if not value:
            print(f"ERROR: missing required FirecREST env var '{key}'", file=sys.stderr)
            sys.exit(1)
        env_vars[key] = value.strip()
    return env_vars

def create_firecrest_client(fc_env):
    auth = StaticTokenAuth(fc_env["FIRECREST_TOKEN"])
    client = fc.v2.Firecrest(
        firecrest_url=fc_env["FIRECREST_URL"],
        authorization=auth,
        verify=True,
    )
    return client

def collect_s3_env():
    keys = [
        "AWS_ACCESS_KEY_ID",
        "AWS_SECRET_ACCESS_KEY",
        "S3_ENDPOINT_URL",
        "S3_REGION",
        ]

    env_vars = {}

    for key in keys:
        value = os.environ.get(key)
        if not value:
            print(f"ERROR: missing required S3 env var '{key}'", file=sys.stderr)
            sys.exit(1)
        env_vars[key] = value.strip()

    return env_vars

def collect_mlflow_env():
    keys = [
        "MLFLOW_TRACKING_URI",
        "MLFLOW_TRACKING_USERNAME",
        "MLFLOW_TRACKING_PASSWORD",
    ]

    env_vars = {}

    for key in keys:
        value = os.environ.get(key)
        if not value:
            print(
                f"ERROR: missing required MLflow env var '{key}'",
                file=sys.stderr,
            )
            sys.exit(1)
        env_vars[key] = value.strip()

    return env_vars

def download_from_s3(uri: str, dst_path: Path) -> None:
    if not uri.startswith("s3://"):
        raise ValueError(f"Unsupported S3 URI: {uri}")

    no_scheme = uri[len("s3://"):]
    if "/" not in no_scheme:
        raise ValueError(f"Invalid S3 URI (missing key): {uri}")

    bucket, key = no_scheme.split("/", 1)

    s3 = boto3.client(
        "s3",
        endpoint_url=os.environ.get("S3_ENDPOINT_URL"),
        region_name=os.environ.get("S3_REGION"),
    )

    obj = s3.get_object(Bucket=bucket, Key=key)
    body = obj["Body"].read()
    dst_path.write_bytes(body)

    if dst_path.stat().st_size == 0:
        raise RuntimeError(f"Downloaded file from {uri} is empty.")



def patch_trainjob_annotation(job_id: str):
    import requests

    k8s_host = os.environ["KUBERNETES_SERVICE_HOST"]
    k8s_port = os.environ["KUBERNETES_SERVICE_PORT"]

    namespace = open("/var/run/secrets/kubernetes.io/serviceaccount/namespace").read().strip()
    token = open("/var/run/secrets/kubernetes.io/serviceaccount/token").read().strip()

    trainjob_name = os.environ.get("TRAINJOB_NAME")
    if not trainjob_name:
        print("WARNING: TRAINJOB_NAME missing, skipping annotation patch")
        return

    url = f"https://{k8s_host}:{k8s_port}/apis/trainer.kubeflow.org/v1alpha1/namespaces/{namespace}/trainjobs/{trainjob_name}"

    headers = {
        "Authorization": f"Bearer {token}",
        "Content-Type": "application/merge-patch+json",
    }

    payload = {
        "metadata": {
            "annotations": {
                "remote.trainer.kubeflow.org/job-id": str(job_id)
            }
        }
    }

    try:
        resp = requests.patch(url,
                              json=payload,
                              headers=headers,
                              verify="/var/run/secrets/kubernetes.io/serviceaccount/ca.crt",
                              )

        if resp.status_code >= 300:
            print(
                f"WARNING: annotation patch failed: "
                f"{resp.status_code} {resp.text}"
            )
    except Exception as e:
        print(f"WARNING: failed to patch annotation: {e}")


def main():

    # --- Inputs and configuration ---

    slurm_uri = require_env("SLURM_URI")
    if not slurm_uri.startswith("s3://"):
        print(
            f"ERROR: SLURM_URI must be an s3:// URI, got: {slurm_uri}",
            file=sys.stderr,
        )
        sys.exit(1)

    fc_env = collect_firecrest_env()
    client = create_firecrest_client(fc_env)

    machine = fc_env["FIRECREST_MACHINE"]
    remote_path = fc_env["FIRECREST_REMOTE_PATH"]
    account = fc_env["FIRECREST_ACCOUNT"]

    # --- Prepare job payload: script, SLURM submission script and runtime environment ---

    with tempfile.TemporaryDirectory() as td:
        td = Path(td)

        s3_env = collect_s3_env()
        mlflow_env = collect_mlflow_env()

        # --- SCRIPTS ---
        script_uri = os.environ.get("SCRIPT_URI")
        script_path = os.environ.get("SCRIPT_PATH")

        if not script_uri and not script_path:
            print("ERROR: neither SCRIPT_URI nor SCRIPT_PATH provided", file=sys.stderr)
            sys.exit(1)

        if script_uri:
            script_name = Path(script_uri).name
        else:
            script_name = Path(script_path).name

        slurm_name = Path(slurm_uri).name

        local_py = td / script_name
        local_slurm = td / slurm_name
        local_env = td / ".env"

        if script_uri:
            print(f"Downloading user script from {script_uri} ...")
            try:
                download_from_s3(script_uri, local_py)
            except Exception as e:
                print(f"FATAL ERROR: {e}", file=sys.stderr)
                sys.exit(1)

            if not local_py.exists() or local_py.stat().st_size == 0:
                print("FATAL ERROR: downloaded file missing or empty", file=sys.stderr)
                sys.exit(1)

            print("Download OK.")

        else:
            if not os.path.exists(script_path):
                print(f"ERROR: Script not found at {script_path}", file=sys.stderr)
                sys.exit(1)

            with open(script_path, "r") as f:
                local_py.write_text(f.read(), encoding="utf-8")

        # --- ENV ---

        env_vars = {
            **s3_env,
            **collect_mlflow_env(),
}
        lines = [f"export {k}='{v}'" for k, v in s3_env.items()]
        local_env.write_text("\n".join(lines))

        # --- SLURM ---
        print(f"Downloading SLURM script from {slurm_uri} ...")
        download_from_s3(slurm_uri, local_slurm)
        print("Download OK.")

        # --- Create folder for logs

        now = datetime.now()

        date_dir = now.strftime("%Y-%m-%d")
        day_path = f"{remote_path}/kf_output/{date_dir}"

        run_id = now.strftime("%H-%M-%S") + "_" + uuid.uuid4().hex[:6]
        job_dir = f"{day_path}/{run_id}"

        print("Creating remote working directory...")
        try:
            client.mkdir(machine, job_dir, create_parents=True)
        except FirecrestException as e:
            if "File exists" not in str(e):
                raise
        print("Remote working directory created.")

        # --- Upload files to the remote working directory ---
        print(f"Uploading {script_name}, {slurm_name} and .env to {machine}:{job_dir} ...")
        client.upload(
            system_name=machine,
            local_file=str(local_py),
            directory=job_dir,
            filename=script_name,
            account=account,
            blocking=True,
        )

        client.upload(
            system_name=machine,
            local_file=str(local_slurm),
            directory=job_dir,
            filename=slurm_name,
            account=account,
            blocking=True,
        )

        client.upload(
            system_name=machine,
            local_file=str(local_env),
            directory=job_dir,
            filename=".env",
            account=account,
            blocking=True,
        )

        print("Job files uploaded successfully.")

        # --- Submit SLURM job ---
        remote_slurm_path = f"{job_dir}/{slurm_name}"
        print(f"Submitting job: {remote_slurm_path}")

        job = client.submit(
            machine,
            working_dir=job_dir,
            script_remote_path=remote_slurm_path,
            account=account,
        )

        jobid = None
        if isinstance(job, dict):
            jobid = job.get("jobid") or job.get("jobId") or job.get("job_id")
        if not jobid:
            print(f"ERROR: submit response missing jobid: {job}", file=sys.stderr)
            sys.exit(1)

        print(f"Submitted jobid={jobid}")

        patch_trainjob_annotation(jobid)

        # --- Poll for completion ---
        print("Waiting for job to finish...")
        result = client.wait_for_job(machine, jobid)

        state = None
        if isinstance(result, list) and len(result) > 0:
            state = result[0].get("status", {}).get("state")

        print("Final state:", state)

        if state != "COMPLETED":
            print(f"ERROR: job {jobid} finished with state={state}", file=sys.stderr)
            sys.exit(1)

        print(f"Job {jobid} completed successfully.")

        # ---- Fetch logs ----
        out_file = f"{job_dir}/firecrest-{jobid}.out"
        err_file = f"{job_dir}/firecrest-{jobid}.err"

        print("\n--- REMOTE STDOUT ---")
        try:
            print(client.view(machine, out_file))
        except Exception as e:
            print(f"(Could not fetch stdout: {e})")

        print("\n--- REMOTE STDERR ---")
        try:
            print(client.view(machine, err_file))
        except Exception as e:
            print(f"(Could not fetch stderr: {e})")

        print("Remote job completed successfully.")
        sys.exit(0)

if __name__ == "__main__":
    try:
        main()
    except Exception as e:
        print(f"FATAL ERROR: {e}", file=sys.stderr)
        sys.exit(1)