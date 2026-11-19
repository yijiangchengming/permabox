# Deployment writable layers

Deployment replicas can opt into a node-local pool managed by the OCI runtime. Kubernetes keeps scheduling Pods. A replacement either receives an available, compatible layer on its scheduled node or starts with an empty layer. StatefulSet and per-Pod behavior remain unchanged.

Operators explicitly allow trusted namespaces. Owner references verify object identity and lifecycle, but do not prove access authorization in a namespace shared by untrusted tenants.

Deployment UID, container name, runtime and actual image identity isolate pools. Each claim records a Pod name and UID. Only the same Pod or a replacement whose predecessor is confirmed finished or absent by the Kubernetes API can claim a released layer. Journals and mount checks also have to show that the previous container released the layer. Discovery can use remote reads outside the node lock; selection, rechecking the release marker, and preparing the mount run under the same lock as reclamation. Unknown image identity disables reuse. No layer is transferred between nodes.

Workload deletion and scale to zero use the Deployment, independent of ReplicaSet rollouts. The existing policy and idle TTL apply to each released layer. Workloads from different deployments and nodes never exchange layers.

Implementation and verification:

1. Extend the metadata bridge and add a trusted lookup for the actual CRI container image.
2. Add an exclusive, durable slot claim to the mount lifecycle and cover same-Pod preference, concurrent claims and released-layer safety.
3. Connect runtime and Helm opt-ins and document the scheduling and persistence guarantees in both languages.
4. Exercise multi-replica replacement, scaling, and GC on the K3s Actions runner, using the current installer image.
