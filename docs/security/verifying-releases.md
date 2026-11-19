# Verifying Meitheal releases

Every released image (`ghcr.io/defilantech/meitheal-operator`,
`ghcr.io/defilantech/meitheal-agent` and
`ghcr.io/defilantech/meitheal-agent-coder`) is signed with cosign (keyless,
Sigstore) and carries an SLSA build provenance attestation and an SBOM
attestation. The `.tar.gz` release archives carry an SLSA build provenance
attestation. All of them are signed by one identity: the `release-please.yml`
workflow of `defilantech/meitheal`, running on `refs/heads/main`.

The commands below accept that identity and nothing else. A signature made by
another workflow, by another repository under the same owner, or by this
workflow run from any other branch fails them.

Every release archive, and the operator and agent images, also carry the
license and NOTICE files of the third-party Go modules the release links:
`third_party_licenses/` next to `LICENSE` in each archive, and `/licenses/` in
each of those images.

## Verify the image signature

    cosign verify ghcr.io/defilantech/meitheal-operator:<version> \
      --certificate-identity https://github.com/defilantech/meitheal/.github/workflows/release-please.yml@refs/heads/main \
      --certificate-oidc-issuer https://token.actions.githubusercontent.com

Use cosign 3 or later. The release signs with cosign 3, which attaches the
signature to the image as a Sigstore bundle; cosign 2 looks for a `.sig` tag
instead unless it is given `--new-bundle-format`.

## Verify SLSA provenance and SBOM

The provenance and SBOM attestations are produced by `actions/attest-build-provenance`
and `actions/attest-sbom`, which write to the GitHub attestations store rather than
the cosign-native attestation format, so the GitHub CLI is the tool to verify them
with, not `cosign verify-attestation`.

    gh attestation verify oci://ghcr.io/defilantech/meitheal-operator:<version> \
      --repo defilantech/meitheal \
      --signer-workflow defilantech/meitheal/.github/workflows/release-please.yml \
      --source-ref refs/heads/main

    gh attestation verify oci://ghcr.io/defilantech/meitheal-operator:<version> \
      --repo defilantech/meitheal \
      --signer-workflow defilantech/meitheal/.github/workflows/release-please.yml \
      --source-ref refs/heads/main \
      --predicate-type https://spdx.dev/Document/v2.3

The first command checks the SLSA build provenance attestation (the default
predicate type). The second checks the SBOM attestation, whose predicate type
is derived from the SPDX document version syft emits (`SPDX-2.3` as of this
writing); if a future release bumps that version, update the version suffix
above to match.

`--repo` requires the attestation to come from this repository rather than
any repository the owner has, `--signer-workflow` requires the release
workflow to have signed it, and `--source-ref` requires that workflow to have
run on `main`. `gh attestation verify --owner defilantech` accepts an
attestation from any repository the owner has; prefer `--repo`.

The same applies to `meitheal-agent` and `meitheal-agent-coder`. Verify a
`.tar.gz` release archive the same way, the CLI's
(`meitheal_<version>_<os>_<arch>.tar.gz`) or the agent's
(`meitheal-agent_<version>_<os>_<arch>.tar.gz`):

    gh attestation verify meitheal_<version>_linux_amd64.tar.gz \
      --repo defilantech/meitheal \
      --signer-workflow defilantech/meitheal/.github/workflows/release-please.yml \
      --source-ref refs/heads/main

`checksums.txt` carries no attestation. It shows that a download is intact,
not that the release workflow built it.

## The Helm chart

The chart is published as an OCI artifact at
`oci://ghcr.io/defilantech/charts/meitheal`. Install it by version and pin the
images it deploys by digest in production values, after verifying them as
above.

## Verify the macOS binaries

The darwin builds of `meitheal` and `meitheal-agent` are code-signed with the
bundle identifiers `dev.meitheal.cli` and `dev.meitheal.agent`; the agent
embeds an Info.plist. A release built with the project's signing secrets is
signed with a Developer ID certificate and notarized by Apple:

    codesign -dv --verbose=4 meitheal-agent
    spctl --assess --type install -vv meitheal-agent

The first shows `Identifier=dev.meitheal.agent`, `Info.plist entries=9`, the
`runtime` flag and the Developer ID authority; the second reports
`source=Notarized Developer ID`. A bare executable cannot have its
notarization ticket stapled, so Gatekeeper checks it online on first launch.
Notarization is submitted after the release is published and usually
completes within minutes; until then `spctl` rejects the binary. A release
built without the secrets is ad-hoc signed (`Signature=adhoc`). Maintainers:
see [Signing and notarizing the macOS binaries](../contributing/macos-signing.md).
