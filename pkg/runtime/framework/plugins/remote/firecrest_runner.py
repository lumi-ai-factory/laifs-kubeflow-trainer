#!/usr/bin/env python3
"""
FirecREST runner: executes an inline user script on a remote SLURM system.

Requires:
  - SCRIPT_PATH: path to mounted user script (ConfigMap)
  - SLURM_URI:   s3://bucket/key to the SLURM script (mandatory)
  - FirecREST OAuth + endpoint config via env (client_id, client_secret, token_uri, firecrest_url)
  - machine, remote_path, (optional) account

For S3 download (SLURM_URI):
  - AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY (or compatible envs)
  - optionally AWS_SESSION_TOKEN
  - optionally S3_ENDPOINT_URL (e.g. Allas) and S3_REGION
"""
import os
import sys
import time
import tempfile
from pathlib import Path

import firecrest as fc
import boto3

TERMINAL_STATES = {
    "COMPLETED",
    "FAILED",
    "CANCELLED",
    "TIMEOUT",
    "OUT_OF_MEMORY",
}


def require_env(name: str) -> str:
    """Fetch required environment variable or fail fast with a clear message."""
    v = os.environ.get(name)
    if not v:
        print(f"ERROR: missing env var '{name}'", file=sys.stderr)
        sys.exit(1)
    return v.strip()


def download_slurm_from_s3(slurm_uri: str, dst_path: Path) -> None:
    if not slurm_uri.startswith("s3://"):
        raise ValueError(f"Unsupported SLURM_URI: {slurm_uri}")

    no_scheme = slurm_uri[len("s3://"):]
    if "/" not in no_scheme:
        raise ValueError(f"Invalid S3 URI (missing key): {slurm_uri}")

    bucket, key = no_scheme.split("/", 1)

    endpoint_url = os.environ.get("S3_ENDPOINT_URL")
    region = os.environ.get("S3_REGION")

    s3 = boto3.client(
        "s3",
        endpoint_url=endpoint_url,
        region_name=region,
    )

    obj = s3.get_object(Bucket=bucket, Key=key)
    body = obj["Body"].read()
    dst_path.write_bytes(body)

    if dst_path.stat().st_size == 0:
        raise RuntimeError("Downloaded SLURM script is empty.")


def main():

    # --- Inputs and configuration ------------------------------------------------
    # 1) Path to the user script inside the pod (mounted from ConfigMap/volume).
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

    # 2) FirecREST client configuration (typically injected via Kubernetes Secret).
    client_id = require_env("client_id")
    client_secret = require_env("client_secret")
    token_uri = require_env("token_uri")
    firecrest_url = require_env("firecrest_url")
    machine = require_env("machine")
    remote_path = require_env("remote_path")
    account = os.environ.get("account", "").strip() or None

    # --- Prepare payload: user script + download SLURM submission script from LUMI-O --------
    # Read the inline user code and stage files into a local temp directory.
    with open(script_path, "r") as f:
        user_code = f.read()

    with tempfile.TemporaryDirectory() as td:
        td = Path(td)

        local_py = td / "script.py"
        local_slurm = td / "job.slurm"

        local_py.write_text(user_code, encoding="utf-8")

        print(f"Downloading SLURM script from {slurm_uri} ...")
        download_slurm_from_s3(slurm_uri, local_slurm)
        print("Download OK.")

        # --- FirecREST client setup ------------------------------------------------
        # Use OAuth client-credentials flow to obtain tokens for FirecREST requests.
        auth = fc.ClientCredentialsAuth(client_id, client_secret, token_uri)
        client = fc.v2.Firecrest(firecrest_url=firecrest_url, authorization=auth)

        # --- Upload files to the remote working directory --------------------------
        # Upload both the user script and the SLURM job script to remote_path.
        # blocking=True ensures we don't submit before upload finishes.
        print(f"Uploading script.py and job.slurm to {machine}:{remote_path} ...")
        client.upload(
            system_name=machine,
            local_file=str(local_py),
            directory=remote_path,
            filename="script.py",
            account=account,
            blocking=True,
        )
        client.upload(
            system_name=machine,
            local_file=str(local_slurm),
            directory=remote_path,
            filename="job.slurm",
            account=account,
            blocking=True,
        )
        print("Upload OK.")

        # --- Submit SLURM job ------------------------------------------------------
        # Submit the job script from remote_path and extract the job id from response.
        remote_slurm_path = f"{remote_path}/job.slurm"
        print(f"Submitting job: {remote_slurm_path}")
        job = client.submit(
            machine,
            working_dir=remote_path,
            script_remote_path=remote_slurm_path,
            account=account,
        )
        # FirecREST submit response is typically dict-like; accept common jobid keys.
        jobid = None
        if isinstance(job, dict):
            jobid = job.get("jobid") or job.get("jobId") or job.get("job_id")
        if not jobid:
            print(f"ERROR: submit response missing jobid: {job}", file=sys.stderr)
            sys.exit(1)

        print(f"Submitted jobid={jobid}")
        print("Raw submit response:", job)

        # --- Poll for completion ---------------------------------------------------
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


        # ---- FETCH LOGS ----
        out_file = f"{remote_path}/firecrest-{jobid}.out"
        err_file = f"{remote_path}/firecrest-{jobid}.err"

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

"""
        # --- Fetch logs (best-effort) ----------------------------------------------
        # Attempt to print remote stdout/stderr to pod logs for user visibility.
        out_file = f"{remote_path}/firecrest-{jobid}.out"
        err_file = f"{remote_path}/firecrest-{jobid}.err"

        print("\n--- BEGIN REMOTE STDOUT ---")
        try:
            print(client.view(machine, out_file))
        except Exception as e:
            print(f"(Could not view stdout file {out_file}: {e})", file=sys.stderr)
        print("--- END REMOTE STDOUT ---\n")

        print("\n--- BEGIN REMOTE STDERR ---", file=sys.stderr)
        try:
            print(client.view(machine, err_file), file=sys.stderr)
        except Exception as e:
            print(f"(Could not view stderr file {err_file}: {e})", file=sys.stderr)
        print("--- END REMOTE STDERR ---\n", file=sys.stderr)

        print(f"Submitted job {jobid}. Not waiting for completion.")
        sys.exit(0)

"""

if __name__ == "__main__":
    main()
