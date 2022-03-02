{
  namespace:
    $.core.v1.namespace.new($._config.namespace),

  util+:: {
    local containerPort = $.core.v1.containerPort,
    local container = $.core.v1.container,

    defaultPorts::
      [
        containerPort.newNamed(name='http-metrics', containerPort=$._config.server_http_port),
        containerPort.newNamed(name='grpc', containerPort=9095),
      ],

    readinessProbe::
      container.mixin.readinessProbe.httpGet.withPath('/ready') +
      container.mixin.readinessProbe.httpGet.withPort($._config.server_http_port) +
      container.mixin.readinessProbe.withInitialDelaySeconds(15) +
      container.mixin.readinessProbe.withTimeoutSeconds(1),

    /**
     * @function serviceForUsingNamedPorts
     * @description Generate a Service from an App resource.
     * All ports for all containers are exposed in the Service.
     * Named arguments match the signature defined in (github.com/grafana/jsonnet-libs/ksonnet-util/util.libsonnet).serviceFor
     * but use camelCase naming.
     * This function asserts behavior that serviceFor assumes, notably that each container port is named.
     * @param {DaemonSet|Deployment|ReplicaSet|StatefulSet} app
     * @param {string[]} [ignoredLabels=[]] -
     * Label names that should be ignored when deriving a selector for the Service.
     * @param {string} [nameFormat='%(container)s-%(port)s' -
     * Format for the Service port names.
     * The format string is interpolated with an object with 'container' and 'port' members
     * for API compatibility with the 'serviceFor' function.
     * @returns {Service}
     */
    serviceForUsingNamedPorts(app, ignoredLabels=[], nameFormat='%(container)s-%(port)s')::
      local container = $.core.v1.container;
      local service = $.core.v1.service;
      local ports =
        std.mapWithIndex(
          function(i, c)
            std.mapWithIndex(
              function(j, p)
                {
                  assert std.objectHas(p, 'name') : |||
                    serviceForUsingNamedPorts: all container ports must have a name.
                    .spec.container[%d].ports[%d] is missing a name.
                  ||| % [i, j],
                  name: nameFormat % { container: c.name, port: p.name },
                  port: p.containerPort,
                  targetPort: p.name,
                  [if std.objectHas(p, 'protocol') then 'protocol']: p.protocol,
                },
              ({ ports: [] } + c).ports,
            ),
          app.spec.template.spec.containers
        );
      local selector =
        local labels = app.spec.template.metadata.labels;
        {
          [x]: labels[x]
          for x in std.objectFields(labels)
          if !std.member(ignoredLabels, x)
        };

      service.new(
        name=app.metadata.name,
        selector=selector,
        ports=ports,
      )
      + service.metadata.withLabels({ name: app.metadata.name }),
  },
}
