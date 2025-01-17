## kumactl generate zone-ingress-token

Generate Zone Ingress Token

### Synopsis

Generate Zone Ingress Token that is used to prove Zone Ingress identity.

```
kumactl generate zone-ingress-token [flags]
```

### Examples

```

Generate token bound by zone
$ kumactl generate zone-ingress-token --zone zone-1

```

### Options

```
  -h, --help          help for zone-ingress-token
      --zone string   name of the zone where ingress resides
```

### Options inherited from parent commands

```
      --config-file string   path to the configuration file to use
      --log-level string     log level: one of off|info|debug (default "off")
  -m, --mesh string          mesh to use (default "default")
      --no-config            if set no config file and config directory will be created
```

### SEE ALSO

* [kumactl generate](kumactl_generate.md)	 - Generate resources, tokens, etc

