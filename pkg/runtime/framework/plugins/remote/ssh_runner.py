#!/usr/bin/env python3
import os
import sys
import paramiko

def main():
    """
    Entry point for the remote executor.
    This script runs inside the trainer container and:
      1) Reads the Python code mounted into the pod
      2) Connects to a remote VM over SSH using a private key
      3) Uploads the script to the remote host
      4) Executes the script and streams logs back to Kubernetes
    """

    # -------------------------------------------------------------------------
    # 1. Locate the user script produced by the CustomTrainer
    # -------------------------------------------------------------------------
    script_file_path = os.environ.get("SCRIPT_PATH")
    if not script_file_path:
        print("ERROR: SCRIPT_PATH not set", file=sys.stderr)
        sys.exit(1)

    if not os.path.exists(script_file_path):
        print(f"ERROR: Script not found at {script_file_path}", file=sys.stderr)
        sys.exit(1)

    with open(script_file_path, "r") as f:
        user_python_script = f.read()

    # -------------------------------------------------------------------------
    # 2. Retrieve SSH connection settings from environment variables
    #
    # These values are configured via the ClusterTrainingRuntime manifest.
    # -------------------------------------------------------------------------
    remote_host = os.environ.get("SSH_HOST")
    remote_user = os.environ.get("SSH_USER")
    private_key_path = os.environ.get("SSH_KEY_PATH")
    remote_port = int(os.environ.get("SSH_PORT", "22"))

    if not all([remote_host, remote_user, private_key_path]):
        print("ERROR: SSH_HOST, SSH_USER, SSH_KEY_PATH must be set", file=sys.stderr)
        sys.exit(1)

    # -------------------------------------------------------------------------
    # 3. Load the SSH private key for authentication
    # -------------------------------------------------------------------------
    try:
        ssh_key = paramiko.RSAKey.from_private_key_file(private_key_path)
    except Exception as e:
        print(f"ERROR: Failed loading SSH key: {e}", file=sys.stderr)
        sys.exit(1)

    # -------------------------------------------------------------------------
    # 4. Establish SSH connection to the remote VM
    # -------------------------------------------------------------------------
    ssh_client = paramiko.SSHClient()
    ssh_client.set_missing_host_key_policy(paramiko.AutoAddPolicy())

    print(f"Connecting to {remote_user}@{remote_host}:{remote_port}...")
    try:
        ssh_client.connect(
            hostname=remote_host,
            port=remote_port,
            username=remote_user,
            pkey=ssh_key,
            timeout=20,
        )
        print("SSH connection OK.")
    except Exception as e:
        print(f"ERROR: SSH connection failed: {e}", file=sys.stderr)
        sys.exit(1)

    # -------------------------------------------------------------------------
    # 5. Upload user script to remote /tmp directory
    # -------------------------------------------------------------------------
    remote_script_path = "/tmp/remote_script.py"
    print(f"Uploading script to {remote_script_path}...")

    try:
        sftp = ssh_client.open_sftp()
        with sftp.file(remote_script_path, "w") as remote_file:
            remote_file.write(user_python_script)
        sftp.chmod(remote_script_path, 0o755)
        sftp.close()
    except Exception as e:
        print(f"ERROR: SFTP upload failed: {e}", file=sys.stderr)
        sys.exit(1)

    # -------------------------------------------------------------------------
    # 6. Execute the uploaded script and stream output back to the pod
    # -------------------------------------------------------------------------
    print("Running script on remote host...\n--- BEGIN REMOTE OUTPUT ---")

    try:
        _, stdout, stderr = ssh_client.exec_command(f"python3 {remote_script_path}")
        for line in stdout:
            print(line, end="")
        for line in stderr:
            print(line, end="", file=sys.stderr)
    except Exception as e:
        print(f"ERROR: remote execution failed: {e}", file=sys.stderr)
        sys.exit(1)

    print("--- END REMOTE OUTPUT ---")

    # -------------------------------------------------------------------------
    # 7. Cleanup
    # -------------------------------------------------------------------------
    ssh_client.close()


if __name__ == "__main__":
    main()
