let GitHubActions = (./imports.dhall).GitHubActions

let shellcheck = ./jobs/shellcheck.dhall

let shfmt = ./jobs/shfmt.dhall

let checkPipeline = ./jobs/check-rendered-pipeline-up-to-date.dhall

let dhallFormat = ./jobs/dhall-format.dhall

let dhallLint = ./jobs/dhall-lint.dhall

let prettier = ./jobs/prettier.dhall

let golangci-lint = ./jobs/golangci-lint.dhall

let goreleaser = ./jobs/goreleaser.dhall

in  GitHubActions.Workflow::{
    , name = "CI"
    , on = GitHubActions.On::{ push = Some GitHubActions.Push::{=} }
    , jobs = toMap
        { shellcheck
        , shfmt
        , dhallFormat
        , dhallLint
        , checkPipeline
        , prettier
        , golangci-lint
        , goreleaser
        }
    }
