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
import time
import uuid
import tempfile
from pathlib import Path

import firecrest as fc
from firecrest import FirecrestException
import boto3


def require_env(name: str) -> str:
    """Fetch required environment variable or fail fast with a clear message."""
    v = os.environ.get(name)
    if not v:
        print(f"ERROR: missing env var '{name}'", file=sys.stderr)
        sys.exit(1)
    return v.strip()

def collect_s3_env():
    keys = [
        "AWS_ACCESS_KEY_ID",
        "AWS_SECRET_ACCESS_KEY",
        "S3_ENDPOINT_URL",
        "S3_REGION",]
    env_vars = {}
    for key in keys:
        value = os.environ.get(key)
        if not value:
            print(f"ERROR: missing required S3 env var '{key}'", file=sys.stderr)
            sys.exit(1)
        env_vars[key] = value.strip()
    return env_vars

def download_slurm_from_s3(slurm_uri: str, dst_path: Path) -> None:
    if not slurm_uri.startswith("s3://"):
        raise ValueError(f"Unsupported SLURM_URI: {slurm_uri}")

    no_scheme = slurm_uri[len("s3://"):]
    if "/" not in no_scheme:
        raise ValueError(f"Invalid S3 URI (missing key): {slurm_uri}")

    bucket, key = no_scheme.split("/", 1)

    endpoint_url = os.environ.get("S3_ENDPOINT_URL")
    region_name = os.environ.get("S3_REGION")

    s3 = boto3.client(
        "s3",
        endpoint_url=endpoint_url,
        region_name=region_name,
    )

    obj = s3.get_object(Bucket=bucket, Key=key)
    body = obj["Body"].read()
    dst_path.write_bytes(body)

    if dst_path.stat().st_size == 0:
        raise RuntimeError("Downloaded SLURM script is empty.")


def main():

    # --- Inputs and configuration ---
    script_path = require_env("SCRIPT_PATH")
    if not os.path.exists(script_path):
        print(f"ERROR: Script not found at {script_path}",
        file=sys.stderr,
        )
        sys.exit(1)

    slurm_uri = require_env("SLURM_URI")
    if not slurm_uri.startswith("s3://"):
        print(
            f"ERROR: SLURM_URI must be an s3:// URI, got: {slurm_uri}",
            file=sys.stderr,
        )
        sys.exit(1)

    client_id = require_env("client_id")
    client_secret = require_env("client_secret")
    token_uri = require_env("token_uri")
    firecrest_url = require_env("firecrest_url")
    machine = require_env("machine")
    remote_path = require_env("remote_path")
    account = os.environ.get("account", "").strip() or None

    # --- Prepare payload: user script + download SLURM submission script + from LUMI-O + .env with AWS secrets ---
    with open(script_path, "r") as f:
        user_code = f.read()

    with tempfile.TemporaryDirectory() as td:
        td = Path(td)

        s3_env = collect_s3_env()

        local_py = td / "script.py"
        local_slurm = td / "job.slurm"
        local_env = td / ".env"

        local_py.write_text(user_code, encoding="utf-8")

        lines = [f"export {k}='{v}'" for k, v in s3_env.items()]
        local_env.write_text("\n".join(lines))

        print(f"Downloading SLURM script from {slurm_uri} ...")
        download_slurm_from_s3(slurm_uri, local_slurm)
        print("Download OK.")

        # --- FirecREST client setup ---
        auth = fc.ClientCredentialsAuth(client_id, client_secret, token_uri)
        client = fc.v2.Firecrest(firecrest_url=firecrest_url, authorization=auth)

        # --- Create folder for logs

        now = datetime.now()

        date_dir = now.strftime("%Y-%m-%d")
        day_path = f"{remote_path}/{date_dir}"

        run_id = now.strftime("%H-%M-%S") + "_" + uuid.uuid4().hex[:6]
        job_dir = f"{day_path}/{run_id}"

        try:
            client.mkdir(machine, day_path)
        except FirecrestException as e:
            if "File exists" not in str(e) and e.status_code != 409:
                raise
        client.mkdir(machine, job_dir)

        # --- Upload files to the remote working directory ---
        print(f"Uploading script.py, job.slurm and .env to {machine}:{job_dir} ...")
        client.upload(
            system_name=machine,
            local_file=str(local_py),
            directory=job_dir,
            filename="script.py",
            account=account,
            blocking=True,
        )
        print("Uploaded script.py")
        client.upload(
            system_name=machine,
            local_file=str(local_slurm),
            directory=job_dir,
            filename="job.slurm",
            account=account,
            blocking=True,
        )
        print("Uploaded job.slurm")
        client.upload(
            system_name=machine,
            local_file=str(local_env),
            directory=job_dir,
            filename=".env",
            account=account,
            blocking=True,
        )
        print("Uploaded .env")

        print("Upload OK.")

        # --- Submit SLURM job ---
        remote_slurm_path = f"{job_dir}/job.slurm"
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
        print("Raw submit response:", job)

        # --- Poll for completion ---
        print("Waiting for job to finish...")
        result = client.wait_for_job(machine, jobid)
        print("wait_for_job result:", result)

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
    main()
