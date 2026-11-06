# khaojai/multica fork

This is a public fork of multica-ai/multica. It carries one patch, Microsoft Entra login plus a `workflow_dispatch` trigger on `release.yml`, kept as a short linear commit series on the branch `khaojai/entra`, based on upstream tag v0.8.0. Everything else is upstream unchanged. The fork's `main` carries only the fork's own workflows (`sync-upstream.yml`, `mirror-to-ecr.yml`) and is not used for builds.

## Branches and tags

`khaojai/entra` holds the patch commits on top of an upstream release tag. A tag `vX.Y.Z-khaojai` is upstream `vX.Y.Z` with those commits cherry-picked on top. Pushing it triggers `release.yml`, which builds `ghcr.io/khaojai/multica-backend` and `ghcr.io/khaojai/multica-web` tagged with that name. A tag `vX.Y.Z-khaojai.N` (N = 1, 2, ...) is a rebuild of the same upstream version after a patch fix.

Upstream tags are never pushed to this repo. A bare `vX.Y.Z` tag here would build unpatched code. The sync workflow fetches them into `refs/upstream-tags/*` only inside the runner.

## Automatic sync

`.github/workflows/sync-upstream.yml` runs daily at 02:17 UTC and on manual dispatch. It picks the newest stable upstream tag (v0.8.0 or later), or the `upstream_tag` input if given. If origin already has `<tag>-khaojai` or `<tag>-khaojai.N` it does nothing. Otherwise it cherry-picks the `khaojai/entra` commits onto the upstream tag, tags the result `<tag>-khaojai`, pushes only that tag, then runs `release.yml` on it with `gh workflow run --ref <tag>`. The dispatch is needed because a tag pushed with `GITHUB_TOKEN` does not trigger workflows, and the khaojai org has deploy keys disabled. A cherry-pick conflict fails the job before anything is pushed.

After each successful `Release` run (through its `workflow_run` trigger, and also dispatched by the sync job once the build it started finishes), `.github/workflows/mirror-to-ecr.yml` copies both multi-arch images to ECR `multica/backend` and `multica/web` in account 143495498933 (ap-southeast-1), using the OIDC role `github-actions-khaojai-multica-ecr-push`. That role only trusts `refs/heads/main`, so the workflow has to run from main. A tag that already exists in ECR is skipped, and runs on refs that are not `vX.Y.Z-khaojai[.N]` tags are ignored. To mirror a tag by hand:

    gh workflow run mirror-to-ecr.yml -R khaojai/multica -f tag=vX.Y.Z-khaojai

GitHub disables scheduled workflows after 60 days without repository activity. If the daily run stops, re-enable it in the Actions tab, or run it by hand.

## One-time setup

1. Disable workflows that only make sense upstream:

       gh workflow disable ci.yml -R khaojai/multica
       gh workflow disable mobile-verify.yml -R khaojai/multica
       gh workflow disable openclaw-config-smoke.yml -R khaojai/multica

## Resolving a conflict

When the sync job fails on a cherry-pick, rebase the patch onto the new upstream tag locally:

    git fetch upstream --no-tags 'refs/tags/vX.Y.Z:refs/upstream-tags/vX.Y.Z'
    git checkout khaojai/entra
    git rebase refs/upstream-tags/vX.Y.Z
    git push --force-with-lease origin khaojai/entra

Then rerun the sync workflow with `upstream_tag` set to `vX.Y.Z`. Keep upstream tags out of `refs/tags` locally too, so they cannot be pushed by accident.

## Rebuilding after a patch change

The sync workflow skips versions that already have any `-khaojai` or `-khaojai.N` tag, so a patch fix for an already built version is tagged by hand. Push the fix to `khaojai/entra`, then:

    git fetch upstream --no-tags 'refs/tags/v0.9.0:refs/upstream-tags/v0.9.0'
    git checkout --detach refs/upstream-tags/v0.9.0
    git cherry-pick "$(git merge-base refs/upstream-tags/v0.9.0 origin/khaojai/entra)..origin/khaojai/entra"
    git tag -a v0.9.0-khaojai.1 -m "Upstream v0.9.0 with khaojai/entra patch"
    git push origin refs/tags/v0.9.0-khaojai.1

## Deployment

The deployment lives in github.com/insightblue-co/argocd-apps under `gitops/multica`. The ghcr packages stay private. insightblue pulls the images from the ECR copies, and argocd-image-updater picks up new `-khaojai` tags there.
