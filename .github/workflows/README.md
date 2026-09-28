
# Build and Publish Snap Package workflow

This workflow builds a `snap` package for a release tag with `release/package-linux.sh` and, when the `publish` input is set, uploads it to the [Snap Store](https://snapcraft.io/store).  It has a `workflow_dispatch` trigger so it can be triggered manually only, it is not hooked up to any other GH event.  The tag's XMR library must already be published (`wallet/asset/xmr/publish-libs.sh`), since the workflow fetches it with `fetch-libs.sh`.

The store upload requires the `SNAPCRAFT_STORE_CREDENTIALS` variable to be set in [Github Secrets](https://github.com/bisoncraft/caribou/settings/secrets/actions).  
See <https://github.com/snapcore/action-publish> for details.

More information on Snapcraft authentication: [https://snapcraft.io/docs/snapcraft-authentication]
