# Resource Activation Contract

Status: planning draft

## 1. Ownership and authority

The component repository owns resource inventory and normal activation intent.
The coordination backend owns sparse composition-specific overrides. An
immutable Release owns the final resolved activation plan. A target selects a
Release; an Environment does not override activation in v1.

A component change cannot affect a target until an authorized actor creates a
Release pinning that commit and an authorized target change selects the Release.

## 2. Component layout and manifest

Each FiveM component declares its own detected and accepted layout in
`.fxvcs/repository.yaml`; discovery never depends on coordination access:

```yaml
apiVersion: fxvcs.dev/v1alpha1
kind: Repository
metadata:
  repositoryID: repo_maps_01
  storageDomainID: domain_maps_01
  name: maps
spec:
  profile: fivem
  minimumFXVCSVersion: 0.1.0
  gitIntegrationVersion: 1
  sourceRoot: .
  layout: collection # single | collection
  resourceName: null # required only for single
  storage:
    remotes:
      - name: primary
        type: s3
        endpoint: https://objects.example.com
        bucket: fxvcs-assets
        prefix: repositories/domain_maps_01/
        requiredForPublication: true
```

`fxvcs init --preset fivem` proposes this descriptor. `stack add` reads it
and may record an explicit composition override. A disagreement between the
pinned component descriptor and stack override is shown and fails closed
until reviewed. Mount paths remain composition-owned.

`metadata.name` is the component's display name. Coordination requires every
resolved `repositoryName` to be unique within one composition. If two component
repositories choose the same name, the coordination repository record supplies
a unique alias keyed by `repositoryID`; it never rewrites component identity.
A Release snapshots that resolved display name, but names and aliases are
excluded from `sourceSetDigest`, so a display-only rename does not change source
identity.

The component inventory lives in `.fxvcs/resources.yaml`:

```yaml
apiVersion: fxvcs.dev/v1alpha1
kind: ResourceCatalog
metadata:
  repositoryID: repo_maps_01
spec:
  resources:
    - path: city-map
      name: city-map
      activation: enabled # enabled | disabled | pending
      after: [mapmanager]
      before: []
      provides: []
      dependencies: [mapmanager]
      games: [gta5]
      serverOnly: false
      persistentPaths: []
      persistentPathKinds: {} # each declared path: file | directory
      mutableMode: immutable # immutable | copy
      status: present # present | missing
```

Stable identity is `(repositoryID, normalized resource-relative path)`. Names
are display/runtime identifiers and are validated globally; they are not keys.
`pending` is explicit and never represented by YAML null or a missing field.
`missing` is a review tombstone produced by synchronization and cannot enter a
Release.

## 3. Discovery and manifest evaluation

`fxvcs resources sync` parses every `fxmanifest.lua` and legacy
`__resource.lua` below the component's accepted source root. It preserves
operator-owned activation, ordering, annotations, persistent-path fields/kinds, and
`mutableMode` for matching identities; new entries default to `pending`; missing
directories are marked `missing`; it never commits.

Manifests are Lua programs, not plain data. V1 evaluates them in a deterministic
sandbox with:

- no filesystem, network, subprocess, native module, package loader, clock, or
  random access;
- only allowlisted manifest functions and basic pure Lua operations;
- instruction, memory, recursion, output, and wall-clock limits;
- deterministic ordering and fail-closed behavior for unsupported effects.

The evaluator records at least resource name/path, `provide`, `dependency` and
`dependencies`, `server_only`, and `games`. Unknown or dynamic behavior that
cannot be resolved safely is a validation error, not silently ignored.

Release creation re-scans each pinned Git tree and compares it with the pinned
catalog. A discovered directory absent from the manifest is pending and fails;
a catalog entry without a resource directory is missing and fails; name,
manifest, or descriptor disagreement fails before object download.

## 4. Runtime and external resources

Component catalog entries are implicitly `component`. Resources not supplied by
a component repository live in a coordination-owned
`RuntimeResourceCatalog`, which `fxvcs runtime-resources sync --from-cfg` may
seed in a coordination checkout but never commit:

```yaml
apiVersion: fxvcs.dev/v1alpha1
kind: RuntimeResourceCatalog
spec:
  resources:
    - id: runtime:system/monitor
      name: monitor
      source: runtime
      activation: enabled
    - id: external:host/custom-anticheat
      name: custom-anticheat
      source: external
      activation: enabled
      verifier:
        contract: resource-present
        version: 1
      lifecycle:
        ownership: external
        actions: observe-only
```

Stable non-component IDs are namespaced and unique within the composition:

- `runtime:<provider>/<path>` is supplied by the target-provided FXServer or
  supervisor installation. V1 validates it against the actual installed
  runtime during staging and records the observed runtime evidence; it does not
  claim or hash those bytes as Release-owned source.
- `external:<owner>/<name>` is installed and owned outside FXVCS. The catalog
  owns the portable verifier contract ID/version and lifecycle requirements. The
  target binding must map that contract to one allowlisted local implementation;
  endpoints, credentials, paths, and implementation configuration remain local.

Common `cfx-server-data` resources such as `chat`, `mapmanager`, `spawnmanager`,
`hardcap`, and `sessionmanager` are not FXServer system resources. They should
normally be a vendored, pinned component repository; installations that keep
them outside FXVCS declare them `external`. Runtime entries such as `monitor`
and `webadmin` are accepted only when staging discovers them in the installed
runtime. `--from-cfg` must not infer source kind from a resource's familiar
name; ambiguous entries remain pending for review.

## 5. Coordination overrides and resolution

Coordination stores only sparse overrides, keyed by stable repository ID and
path:

```yaml
apiVersion: fxvcs.dev/v1alpha1
kind: ResourceOverrides
spec:
  resources:
    - repositoryID: repo_tools_01
      path: map-editor
      activation: disabled
      after: []
      before: [admin-tools]
```

Allowed override fields are `activation`, `after`, and `before`. Identity,
source path, source kind, dependencies declared by the resource, provides,
games, and persistent paths cannot be rewritten by coordination. A conflict or
cycle is invalid; FXVCS never chooses one edit over another automatically.

Literal `release create --enable/--disable` flags create a release-only draft
override unless the user explicitly writes the coordination override file.

## 6. Immutable Release plan and variants

Every Release entry carries stable and display identity plus provenance:

```yaml
- source: component
  repositoryID: repo_tools_01
  repositoryName: tools
  path: map-editor
  name: map-editor
  activation: enabled
  activationSource: release-override
- source: runtime
  resourceID: runtime:system/monitor
  name: monitor
  activation: enabled
  activationSource: runtime-catalog
```

`activationSource` is exactly `component`, `runtime-catalog`,
`coordination-override`, or `release-override`. Component identity is
`(repositoryID, path)`; runtime/external identity is `resourceID`. The Release
snapshots resolved order, source kind, aliases, and dependencies. Existing
Releases never consult later manifests or overrides.

Every Release has a mandatory `sourceSetDigest` computed from component IDs,
commit SHAs, asset roots, mount descriptors, and non-component resource IDs plus
canonical portable verifier-contract/lifecycle-requirement digests, but not
display names, aliases, activation, environment state, resolved target verifier
bindings, or target-provided runtime observations. An
optional `variantOf` references another Release for human grouping. Comparison
reports source equality independently from activation differences; separate
dev/prd variants therefore do not obscure whether code and assets are identical.
