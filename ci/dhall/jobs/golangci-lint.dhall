let GitHubActions = (../imports.dhall).GitHubActions

let Setup = ../setup.dhall

let SetupSteps = Setup.SetupSteps

let Job = Setup.Job

in  Job::{
    , name = Some "golangci-lint"
    , steps =
          SetupSteps
        # [ GitHubActions.Step::{
            , name = Some "golangci-lint"
            , uses = Some "golangci/golangci-lint-action@v2.3.0"
            , `with` = Some (toMap { version = "v1.32.2" })
            }
          ]
    }
