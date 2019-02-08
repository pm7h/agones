# Release {version}

<!--
This is the release issue template. Make a copy of the markdown in this page
and copy it into a release issue. Fill in relevent values, found inside {}
!-->

- [ ] Review closed issues have appropriate tags.
- [ ] Review closed issues have been applied to the current milestone.
- [ ] Review closed PRs have appropriate tags.
- [ ] Review closed PRs have been applied to the current milestone.
- [ ] Ensure the next version milestone is created.
- [ ] Any issues in the current milestone that are not closed, move to next milestone.
- [ ] Run `make gen-changelog` to generate the CHANGELOG.md (if release candidate `make gen-changelog RELEASE_VERSION={version}-rc`)
- [ ] Ensure the [helm `tag` value][values] is correct (should be the {version} if a full release, {version}-rc if release candidate)
- [ ] Ensure the [helm `Chart` version values][chart] are correct (should be the {version} if a full release, {version}-rc if release candidate)
- [ ] Run `make gen-install`
- [ ] Ensure all example images exist on gcr.io/agones-images-
- [ ] Create a *draft* release with the [release template][release-template]
  - [ ] Make a `tag` with the release version.
- [ ] Site updated
  - [ ] If full release, review and remove all instances of the `feature` shortcode
  - [ ] Update to the new release branch (`release-branch` in config.toml) to {version}, or {version}-rc if release candidate.
  - [ ] If full release, update site with the new release version (`release-version` in config.toml) to {version}
  - [ ] If full release, update documentation with updated example images tags
  - [ ] Copy the draft release content into a new `/site/content/en/blog/releases` content (this will be what you send via email). 
- [ ] Create PR with these changes, and merge them with approval
- [ ] Confirm local git remote `upstream` points at `git@github.com:GoogleCloudPlatform/agones.git`
- [ ] Run `git remote update && git checkout master && git reset --hard upstream/master` to ensure your code is in line with upstream  (unless this is a hotfix, then do the same, but for the the release branch)
- [ ] Run `make do-release`. (if release candidate: `make do-release RELEASE_VERSION={version}-rc`) to create and push the docker images and helm chart.
- [ ] Do a `helm repo add agones https://agones.dev/chart/stable` and verify that the new version is available via the command `helm search agones/`
- [ ] Do a `helm install` and a smoke test to confirm everything is working.
- [ ] Attach all assets found in the `release` folder to the release.
- [ ] Submit the Release.
- [ ] Run `make site-deploy` (if release candidate: `make site-deploy SERVICE=rc`), and make it the default version
- [ ] Send an email to the [mailing list][list] with the release details (copy-paste the release blog post)
- [ ] If full release, then increment the `base_version` in [`build/Makefile`][build-makefile]
- [ ] Ensure the [helm `tag` value][values] is set to the next version (should be the {version}+0.1 if a full release, {version}+0.1-rc if release candidate)
- [ ] Ensure the [helm `Chart` version values][chart] is set to the next version (should be the {version}+0.1 if a full release, {version} if release candidate)
- [ ] Run `make gen-install`
- [ ] Create PR with these changes, and merge them with approval
- [ ] Close this issue.
- [ ] If full release, close the current milestone. *Congratulations!* - the release is now complete! :tada: :clap: :smile: :+1:

[values]: https://github.com/GoogleCloudPlatform/agones/blob/master/install/helm/agones/values.yaml#L33
[chart]: https://github.com/GoogleCloudPlatform/agones/blob/master/install/helm/agones/Chart.yaml
[list]: https://groups.google.com/forum/#!forum/agones-discuss
[release-template]: https://github.com/GoogleCloudPlatform/agones/blob/master/docs/governance/templates/release.md
[build-makefile]: https://github.com/GoogleCloudPlatform/agones/blob/master/build/Makefile