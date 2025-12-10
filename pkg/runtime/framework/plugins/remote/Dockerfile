# Base image: lightweight Python runtime.
# This container does NOT contain ML frameworks — it only SSH-executes code remotely.
FROM python:3.11-slim

# Install the SSH client library
RUN pip install --no-cache-dir paramiko

# All runtime files live under /app
WORKDIR /app

# Copy the runner script from the build context into the container
COPY ./ssh_runner.py /app/ssh_runner.py

# Ensure the runner is executable inside the container
RUN chmod +x /app/ssh_runner.py

# This container does not run training locally.
# Instead, it executes the ssh_runner.py script, which:
#  - retrieves a user-generated Python script via SCRIPT_PATH
#  - connects via SSH to a remote machine
#  - pushes that script to /tmp on the remote node
#  - executes it and streams output back to Kubernetes
ENTRYPOINT ["python3", "/app/ssh_runner.py"]
