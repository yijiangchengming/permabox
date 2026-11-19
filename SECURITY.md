# Security

PermaBox runs on Kubernetes nodes and the K3s installer has host privileges. Limit installation and access to the `permabox` RuntimeClass to trusted operators and workloads. See the [deployment guide](deploy/README.md) for the storage isolation and image upgrade model.

For suspected vulnerabilities, use GitHub private vulnerability reporting on the repository when available, or contact the repository maintainers privately. Please include the PermaBox version, relevant platform details, and enough information to reproduce the issue. Remove credentials and workload data from the report. Do not open a public issue for an unpatched vulnerability.
