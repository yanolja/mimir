local k = import 'k.libsonnet',
      container = k.core.v1.container,
      deployment = k.apps.v1.deployment,
      daemonSet = k.apps.v1.daemonSet,
      replicaSet = k.apps.v1.replicaSet,
      service = k.core.v1.service,
      statefulSet = k.apps.v1.statefulSet;

local common = k + (import 'common.libsonnet');

local anonymousPort = { containerPort: 8080 };
local namedPort = { name: 'PORT', containerPort: 8080 };
local namedPort2 = { name: 'PORT2', containerPort: 8081 };

local protoContainer = container.new('CONTAINER', 'IMAGE');
local containerWithAnonymousPort = protoContainer + container.withPorts([anonymousPort]);
local containerWithNamedPort = protoContainer + container.withPorts([namedPort]);
local containerWithNamedPort2 = protoContainer + container.withPorts([namedPort2]);
local containerWithMultiplePorts = protoContainer + container.withPorts([namedPort, namedPort2]);

{
  local eval = common.util.serviceForUsingNamedPorts,
  'Deployment/container-with-anonymous-port.error': eval(
    deployment.new('APP', 1, [containerWithAnonymousPort], { name: 'APP' })
  ),
  'Deployment/container-with-named-port.json': eval(
    deployment.new('APP', 1, [containerWithNamedPort], { name: 'APP' })
  ),
  'Deployment/container-with-multiple-ports.json': eval(
    deployment.new('APP', 1, [containerWithMultiplePorts], { name: 'APP' })
  ),
  'Deployment/multiple-containers.json': eval(
    deployment.new('APP', 1, [containerWithNamedPort, containerWithNamedPort2], { name: 'APP' })
  ),


  'DaemonSet/container-with-named-port.json': eval(
    daemonSet.new('APP', [containerWithNamedPort], { name: 'APP' })
  ),
  'ReplicaSet/container-with-named-port.json': eval(
    replicaSet.new('APP')
    + replicaSet.spec.template.spec.withContainers([containerWithNamedPort])
    + replicaSet.spec.template.metadata.withLabels({ name: 'app-name' })
    + replicaSet.spec.withReplicas(1)
  ),
  'StatefulSet/container-with-named-port.json': eval(
    statefulSet.new('app-name', 1, [containerWithNamedPort], { name: 'app-name' })
  ),
}
