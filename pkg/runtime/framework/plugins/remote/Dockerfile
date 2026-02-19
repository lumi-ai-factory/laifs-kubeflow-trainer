# -----------------------------------------------------------------------------
# Remote trainer runtime image
# Executes training scripts remotely via FirecREST + SLURM.
# Does NOT run ML training locally.
# -----------------------------------------------------------------------------

FROM python:3.11-slim

# FirecREST Python client for remote execution
RUN pip install --no-cache-dir pyfirecrest boto3

WORKDIR /runner
COPY ./firecrest_runner.py /runner/firecrest_runner.py
RUN chmod +x /runner/firecrest_runner.py

ENTRYPOINT ["python3", "/runner/firecrest_runner.py"]
