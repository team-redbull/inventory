FROM python:3.11-slim

WORKDIR /app

COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt

COPY common.py .
COPY ome.py .
COPY cisco_intersight.py .
COPY ucscentral.py .

# Run one collector per container — select via CMD override or entrypoint arg.
# Examples:
#   docker run fleet-collectors python ome.py
#   docker run fleet-collectors python cisco_intersight.py
#   docker run fleet-collectors python ucscentral.py
CMD ["python", "--help"]
