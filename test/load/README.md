# Load Testing

Load tests aim to test the performance of the system under heavy load. Game server allocation is an example where multiple parallel operations should be tested.

## Build and run test

Prerequisites:
- Docker.
- a running k8s cluster (kube config is passed as arguments).
- Have kubeconfig file ready.

Load tests are written using Locust. These tests are also integrated with Graphite and Grafana
for storage and visualization of the results. 

To run load tests using Locust on your local machine:

```
docker build -t locust-files .
docker run --rm --network="host" -e "TARGET_HOST=http://127.0.0.1:8001" locust-files:latest
```

The above will build a Docker container to install Locust, Grafana, and Graphite and will configure
them. The test uses the HTTP proxy on the local machine to access the k8s API. 

After running the Docker container you can access Locust on port 8089 of your local machine.
Grafana will be available on port 80, and Graphite on port 81.