-- Add allow_experiments flag to repos table.
-- Only meaningful on argocd-typed repos: gates whether experiment-branch builds
-- (build.branch != source repo's configured main branch) may deploy to the
-- environment served by this argocd repo. Defaults to 0 (false) so production
-- argocd repos refuse experiment deployments unless explicitly opted in.
-- Application-typed rows store the column too but it has no effect there.
ALTER TABLE repos ADD COLUMN allow_experiments INTEGER NOT NULL DEFAULT 0;
