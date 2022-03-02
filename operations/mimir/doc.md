<a name="serviceForUsingNamedPorts"></a>

## serviceForUsingNamedPorts(app, [ignoredLabels], nameFormat) ⇒ <code>Service</code>
Generate a Service resource from either a
DaemonSet, Deployment, ReplicaSet, or StatefulSet.
The Service selector is derived from the .metadata.labels.
All ports for all containers are exposed in the Service.
Named arguments match the signature defined in (github.com/grafana/jsonnet-libs/ksonnet-util/util.libsonnet).serviceFor
but use camelCase naming.
This function asserts behavior that serviceFor assumes, notably that each container port is named.

**Kind**: global function  

| Param | Type | Default | Description |
| --- | --- | --- | --- |
| app | <code>DaemonSet</code> \| <code>Deployment</code> \| <code>ReplicaSet</code> \| <code>StatefulSet</code> |  |  |
| [ignoredLabels] | <code>Array.&lt;string&gt;</code> | <code>[]</code> | Label names that should be ignored when deriving a selector for the Service. |
| nameFormat | <code>string</code> | <code>&quot;&#x27;%(container)s-%(port)s&#x27;&quot;</code> | Format for the Service port names. The format string is interpolated with an object with 'container' and 'port' members for API compatibility with the 'serviceFor' function. |

