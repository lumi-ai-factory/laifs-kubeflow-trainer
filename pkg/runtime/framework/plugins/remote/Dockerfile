# This container does NOT contain ML frameworks — it only executes code remotely via FirecREST.
FROM python:3.11-slim

# FirecREST Python client
RUN pip install --no-cache-dir pyfirecrest boto3

WORKDIR /runner
COPY ./firecrest_runner.py /runner/firecrest_runner.py
RUN chmod +x /runner/firecrest_runner.py

ENTRYPOINT ["python3", "/runner/firecrest_runner.py"]
