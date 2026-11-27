# Changelog

## [0.1.2](https://github.com/defilantech/meitheal/compare/v0.1.1...v0.1.2) (2026-11-27)


### Features

* give the intent planner the contents of the files most related to the issue ([#52](https://github.com/defilantech/meitheal/issues/52)) ([873d483](https://github.com/defilantech/meitheal/commit/873d4830eb7b2baa27dab30a8d38d6c20271dd1c))
* lint planned intents against the repository before queueing them ([#50](https://github.com/defilantech/meitheal/issues/50)) ([92d6d58](https://github.com/defilantech/meitheal/commit/92d6d5884fc42263333e685807196373e07f3d5f))


### Bug Fixes

* derive a failed run's park reason from its tasks' verdicts ([#49](https://github.com/defilantech/meitheal/issues/49)) ([a95dbe6](https://github.com/defilantech/meitheal/commit/a95dbe607fd3d8924cd0b434d7053c3b1f919916))
* pull gate Job images with the chart's imagePullSecrets ([#48](https://github.com/defilantech/meitheal/issues/48)) ([4b03ba7](https://github.com/defilantech/meitheal/commit/4b03ba7352a93bee3d14f403e98c9825891780ed))
* re-review an untrusted reviewer NO-GO instead of opening a fix iteration ([#51](https://github.com/defilantech/meitheal/issues/51)) ([01f5c9f](https://github.com/defilantech/meitheal/commit/01f5c9fa7450e8e8357820d38f8c160f9a403b89))
* scope the planner's test rule to behaviour changes and state rule precedence ([#47](https://github.com/defilantech/meitheal/issues/47)) ([a35a458](https://github.com/defilantech/meitheal/commit/a35a458ab908e24f1c263b34bc095c3e7352561a))

## [0.1.1](https://github.com/defilantech/meitheal/compare/v0.1.0...v0.1.1) (2026-11-27)


### Bug Fixes

* build the meitheal cli for windows again ([#39](https://github.com/defilantech/meitheal/issues/39)) ([f779ff4](https://github.com/defilantech/meitheal/commit/f779ff4099f9615fbfe7069bb8f9534d04eae512))

## [0.1.0](https://github.com/defilantech/meitheal/compare/v0.1.0...v0.1.0) (2026-11-27)


### ⚠ BREAKING CHANGES

* the new API group means new CRDs. Meitheal installs beside an existing Foreman release instead of upgrading it; the README's migration section covers moving an existing install.

### Features

* meitheal run drives a queue unattended, with a planner and a GPU gate ([#38](https://github.com/defilantech/meitheal/issues/38)) ([d6520da](https://github.com/defilantech/meitheal/commit/d6520da9486b3ea47e93b93e0ca76756deb138bb))
* rename Foreman to Meitheal ([48d3140](https://github.com/defilantech/meitheal/commit/48d3140d050b48f59dda478d1b6e98e7f019847f))


### Documentation

* add a Celtic knot logo, light and dark lockups and a social preview ([#36](https://github.com/defilantech/meitheal/issues/36)) ([cda5c6e](https://github.com/defilantech/meitheal/commit/cda5c6e3e76dacff151d1952c73821f2b15e0cbc))
* add a migration guide for Foreman users ([#3](https://github.com/defilantech/meitheal/issues/3)) ([d9fb579](https://github.com/defilantech/meitheal/commit/d9fb579dc6808462a6e512a63cad36b2fa9cc231))
