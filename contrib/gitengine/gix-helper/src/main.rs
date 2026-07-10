use std::process::ExitCode;

fn main() -> ExitCode {
    match betterleaks_gix_helper::Args::parse(std::env::args_os().skip(1))
        .and_then(betterleaks_gix_helper::run)
    {
        Ok(()) => ExitCode::SUCCESS,
        Err(error) => {
            eprintln!("betterleaks-gix-helper: {error}");
            ExitCode::FAILURE
        }
    }
}
