use std::borrow::Cow;
use std::cmp::Ordering;
use std::collections::HashMap;
use std::error::Error;
use std::ffi::OsString;
use std::io::{self, BufReader, BufWriter, Write};
use std::path::{Path, PathBuf};
use std::process::Command;

use gix::bstr::ByteSlice;
use gix::diff::blob::platform::prepare_diff::Operation;
use gix::object::tree::diff::{Action, Change};

use crate::wire::{
    self, BEND, BGIN, CEND, CMIT, ERRO, FBEG, FEND, HELO, QUIT, append_bytes, error_payload,
    hello_payload, read_frame, write_frame, write_hunk,
};

type AnyError = Box<dyn Error + Send + Sync + 'static>;

#[derive(Debug)]
struct Unsupported(String);

impl std::fmt::Display for Unsupported {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(&self.0)
    }
}

impl Error for Unsupported {}

/// Parsed process arguments for the protocol helper.
#[derive(Debug, Eq, PartialEq)]
pub struct Args {
    repo: PathBuf,
    ambiguous_rename_fallback: bool,
}

impl Args {
    /// Parse one repository path. Profile extensions are deliberately not
    /// advertised by protocol v1 of this candidate.
    ///
    /// # Errors
    ///
    /// Returns an error unless exactly one repository path is supplied.
    pub fn parse(args: impl IntoIterator<Item = OsString>) -> Result<Self, AnyError> {
        let mut ambiguous_rename_fallback = false;
        let mut filtered = Vec::new();
        for arg in args {
            if arg == "--ambiguous-rename-fallback" {
                if ambiguous_rename_fallback {
                    return Err("--ambiguous-rename-fallback was supplied more than once".into());
                }
                ambiguous_rename_fallback = true;
            } else {
                filtered.push(arg);
            }
        }
        let args = filtered;
        let repo = match args.as_slice() {
            [repo] => repo.clone(),
            [flag, repo] if flag == "--repo" => repo.clone(),
            [flag, repo, command, protocol]
                if flag == "-C"
                    && command == "betterleaks--diff-engine"
                    && protocol == "--protocol=1" =>
            {
                repo.clone()
            }
            _ => {
                return Err(
                    "usage: betterleaks-gix-helper [--repo] <repository> [--ambiguous-rename-fallback]\n       betterleaks-gix-helper -C <repository> betterleaks--diff-engine --protocol=1 [--ambiguous-rename-fallback]"
                        .into(),
                );
            }
        };
        Ok(Self {
            repo: repo.into(),
            ambiguous_rename_fallback,
        })
    }
}

struct Engine {
    repo: gix::Repository,
    diff_cache: gix::diff::blob::Platform,
    mailmap: gix::mailmap::Snapshot,
    oid_len: usize,
    ambiguous_rename_fallback: bool,
}

impl Engine {
    fn open(path: &PathBuf, ambiguous_rename_fallback: bool) -> Result<Self, AnyError> {
        let options = gix::open::Options::isolated()
            .strict_config(true)
            .config_overrides([
                "diff.algorithm=myers",
                "diff.renames=true",
                "diff.renameLimit=1000",
            ]);
        let mut repo = gix::open_opts(path, options)?;

        let config = repo.config_snapshot();
        if config
            .sections_by_name("diff")
            .into_iter()
            .flatten()
            .any(|section| section.value("textconv").is_some())
        {
            return Err(Unsupported("diff.*.textconv requires canonical Git".into()).into());
        }
        if config
            .sections_by_name("diff")
            .into_iter()
            .flatten()
            .any(|section| {
                section.header().subsection_name().is_some() && section.value("algorithm").is_some()
            })
        {
            return Err(Unsupported("diff.*.algorithm requires canonical Git".into()).into());
        }
        let mut mailmap = gix::mailmap::Snapshot::default();
        repo.open_mailmap_into(&mut mailmap)?;

        // This cache is intentionally process-scoped. It avoids repeatedly
        // inflating commits, trees, and blobs across sequential batches.
        repo.object_cache_size(128 << 20);
        let mut diff_cache = repo.diff_resource_cache_for_tree_diff()?;
        diff_cache.options.algorithm = Some(gix::diff::blob::Algorithm::Myers);
        diff_cache
            .options
            .skip_internal_diff_if_external_is_configured = false;
        let oid_len = repo.object_hash().len_in_bytes();
        Ok(Self {
            repo,
            diff_cache,
            mailmap,
            oid_len,
            ambiguous_rename_fallback,
        })
    }

    fn validate_batch(&self, commits: &[Vec<u8>]) -> Result<(), AnyError> {
        for bytes in commits {
            let oid = gix::hash::ObjectId::try_from(bytes.as_slice())?;
            let commit = self.repo.find_commit(oid)?;
            if let Some(encoding) = commit_header(&commit.data, b"encoding")
                && !encoding.eq_ignore_ascii_case(b"utf-8")
                && !encoding.eq_ignore_ascii_case(b"utf8")
            {
                return Err(format!(
                    "unsupported commit {}: message encoding {:?} requires canonical Git",
                    commit.id, encoding
                )
                .into());
            }
        }
        Ok(())
    }

    fn scan_batch(
        &mut self,
        output: &mut impl Write,
        batch_id: u64,
        commits: &[Vec<u8>],
    ) -> Result<Counters, AnyError> {
        let mut counters = Counters::default();
        for oid in commits {
            emit_commit(
                &self.repo,
                &mut self.diff_cache,
                &self.mailmap,
                output,
                batch_id,
                oid,
                self.ambiguous_rename_fallback,
                &mut counters,
            )?;
            self.diff_cache.clear_resource_cache_keep_allocation();
        }
        Ok(counters)
    }
}

#[derive(Default)]
struct Counters {
    commits: u64,
    files: u64,
    hunks: u64,
}

/// Run the synchronous helper until a valid `QUIT` or a terminal error.
///
/// # Errors
///
/// Returns an error for repository access, malformed protocol input, output
/// failure, or any semantic condition that makes the worker terminal.
pub fn run(args: Args) -> Result<(), AnyError> {
    // Startup performs the complete repository-level semantic preflight before
    // HELO, so a repository this candidate cannot represent emits no records.
    let stdin = io::stdin();
    let stdout = io::stdout();
    let mut input = BufReader::with_capacity(64 << 10, stdin.lock());
    let mut output = BufWriter::with_capacity(64 << 10, stdout.lock());
    let Args {
        repo,
        ambiguous_rename_fallback,
    } = args;
    let mut engine = match Engine::open(&repo, ambiguous_rename_fallback) {
        Ok(engine) => engine,
        Err(error) => {
            if let Some(unsupported) = error.downcast_ref::<Unsupported>() {
                let payload = error_payload(5, "preflight", &unsupported.0);
                write_frame(&mut output, ERRO, 0, &payload)?;
                output.flush()?;
                return Ok(());
            }
            return Err(error);
        }
    };
    write_frame(&mut output, HELO, 0, &hello_payload(engine.oid_len))?;
    output.flush()?;

    loop {
        let frame = read_frame(&mut input)?;
        if frame.tag == QUIT {
            if frame.batch_id != 0 || !frame.payload.is_empty() {
                return Err("invalid QUIT frame".into());
            }
            output.flush()?;
            return Ok(());
        }
        let batch_id = frame.batch_id;
        let commits = match wire::decode_batch(&frame, engine.oid_len) {
            Ok(commits) => commits,
            Err(error) => return terminal_error(&mut output, batch_id, "decode request", &error),
        };
        if let Err(error) = engine.validate_batch(&commits) {
            return terminal_error(&mut output, batch_id, "validate batch", error.as_ref());
        }
        write_frame(&mut output, BGIN, batch_id, &[])?;
        let counters = match engine.scan_batch(&mut output, batch_id, &commits) {
            Ok(counters) => counters,
            Err(error) => {
                return terminal_error(&mut output, batch_id, "scan batch", error.as_ref());
            }
        };
        let mut payload = vec![0];
        payload.extend_from_slice(&counters.commits.to_be_bytes());
        payload.extend_from_slice(&counters.files.to_be_bytes());
        payload.extend_from_slice(&counters.hunks.to_be_bytes());
        write_frame(&mut output, BEND, batch_id, &payload)?;
        output.flush()?;
    }
}

fn terminal_error<T>(
    output: &mut impl Write,
    batch_id: u64,
    operation: &str,
    error: &(dyn Error + 'static),
) -> Result<T, AnyError> {
    let message = error.to_string();
    let payload = error_payload(2, operation, &message);
    write_frame(output, ERRO, batch_id, &payload)?;
    output.flush()?;
    Err(message.into())
}

#[expect(
    clippy::too_many_lines,
    reason = "commit metadata and one-parent diff emission form one protocol lifecycle"
)]
#[expect(
    clippy::too_many_arguments,
    reason = "protocol emission and explicit engine mode are one commit lifecycle"
)]
fn emit_commit(
    repo: &gix::Repository,
    diff_cache: &mut gix::diff::blob::Platform,
    mailmap: &gix::mailmap::Snapshot,
    output: &mut impl Write,
    batch_id: u64,
    oid_bytes: &[u8],
    ambiguous_rename_fallback: bool,
    counters: &mut Counters,
) -> Result<(), AnyError> {
    let oid = gix::hash::ObjectId::try_from(oid_bytes)?;
    let commit = repo.find_commit(oid)?;
    let mut payload = Vec::new();
    append_bytes(&mut payload, oid_bytes)?;
    append_bytes(
        &mut payload,
        &normalize_message(commit.message_raw_sloppy()),
    )?;
    if commit_header(&commit.data, b"author").is_some() {
        let author = commit.author()?;
        let author_time = author.time().ok();
        let mapped_author = mailmap.resolve_cow(author);
        append_bytes(&mut payload, mapped_author.name.as_ref())?;
        append_bytes(&mut payload, mapped_author.email.as_ref())?;
        payload.extend_from_slice(&author_time.map_or(0, |time| time.seconds).to_be_bytes());
        let offset_minutes = author_time.map_or(0, |time| time.offset / 60);
        payload.extend_from_slice(&offset_minutes.to_be_bytes());
        payload.push(1);
        payload.push(u8::from(author_time.is_some()));
    } else {
        append_bytes(&mut payload, &[])?;
        append_bytes(&mut payload, &[])?;
        payload.extend_from_slice(&0_i64.to_be_bytes());
        payload.extend_from_slice(&0_i32.to_be_bytes());
        payload.extend_from_slice(&[0, 0]);
    }
    write_frame(output, CMIT, batch_id, &payload)?;
    counters.commits += 1;

    let parent_ids: Vec<_> = commit.parent_ids().map(gix::Id::detach).collect();
    if parent_ids.len() <= 1 {
        let new_tree = commit.tree()?;
        let empty_tree;
        let old_tree = if let Some(parent_id) = parent_ids.first() {
            repo.find_commit(*parent_id)?.tree()?
        } else {
            empty_tree = repo.empty_tree();
            empty_tree
        };
        let mut additions = Vec::new();
        let mut deletions = Vec::new();
        let mut modifications = Vec::new();
        let mut exact_renames = Vec::new();
        // Addition and deletion indices are diff-queue encounter ordinals. Git's
        // fuzzy rename tie behavior depends on retaining this order.
        let mut platform = old_tree.changes()?;
        platform.options(|options| {
            options.track_path();
            let rewrites = gix::diff::Rewrites {
                percentage: None,
                ..gix::diff::Rewrites::default()
            };
            options.track_rewrites(Some(rewrites));
        });
        platform.for_each_to_obtain_tree(&new_tree, |change| {
            match change {
                Change::Addition {
                    location,
                    entry_mode,
                    id,
                    ..
                } if !entry_mode.is_tree() => {
                    additions.push(TreeEntry::new(location, entry_mode, id));
                }
                Change::Deletion {
                    location,
                    entry_mode,
                    id,
                    ..
                } if !entry_mode.is_tree() => {
                    deletions.push(TreeEntry::new(location, entry_mode, id));
                }
                Change::Modification {
                    location,
                    previous_entry_mode,
                    previous_id,
                    entry_mode,
                    id,
                } if !entry_mode.is_tree() && same_git_type(previous_entry_mode, entry_mode) => {
                    modifications.push((
                        TreeEntry::new(location, previous_entry_mode, previous_id),
                        TreeEntry::new(location, entry_mode, id),
                    ));
                }
                Change::Rewrite {
                    source_location,
                    source_entry_mode,
                    source_id,
                    entry_mode,
                    location,
                    id,
                    copy: false,
                    ..
                } if !entry_mode.is_tree() => exact_renames.push((
                    TreeEntry::new(source_location, source_entry_mode, source_id),
                    TreeEntry::new(location, entry_mode, id),
                )),
                _ => {}
            }
            Ok::<_, AnyError>(Action::Continue(()))
        })?;
        for (old, new) in &modifications {
            emit_file(
                b'M',
                Some(old),
                new,
                repo,
                diff_cache,
                output,
                batch_id,
                oid_bytes,
                counters,
            )?;
        }
        for (old, new) in &exact_renames {
            emit_file(
                b'R',
                Some(old),
                new,
                repo,
                diff_cache,
                output,
                batch_id,
                oid_bytes,
                counters,
            )?;
        }
        let (renames, unmatched_additions) = match_renames(
            repo,
            old_tree.id,
            new_tree.id,
            &deletions,
            &additions,
            ambiguous_rename_fallback,
        )?;
        for (deletion, addition) in renames {
            emit_file(
                b'R',
                Some(&deletions[deletion]),
                &additions[addition],
                repo,
                diff_cache,
                output,
                batch_id,
                oid_bytes,
                counters,
            )?;
        }
        for addition in unmatched_additions {
            emit_file(
                b'A',
                None,
                &additions[addition],
                repo,
                diff_cache,
                output,
                batch_id,
                oid_bytes,
                counters,
            )?;
        }
    }

    write_frame(output, CEND, batch_id, &[])?;
    Ok(())
}

fn commit_header<'a>(data: &'a [u8], name: &[u8]) -> Option<&'a [u8]> {
    data.split(|byte| *byte == b'\n')
        .take_while(|line| !line.is_empty())
        .find_map(|line| line.strip_prefix(name)?.strip_prefix(b" "))
}

#[derive(Clone, Debug)]
struct TreeEntry {
    path: Vec<u8>,
    mode: gix::objs::tree::EntryMode,
    oid: gix::hash::ObjectId,
}

impl TreeEntry {
    fn new(
        path: impl AsRef<[u8]>,
        mode: gix::objs::tree::EntryMode,
        oid: impl Into<gix::hash::ObjectId>,
    ) -> Self {
        Self {
            path: path.as_ref().to_vec(),
            mode,
            oid: oid.into(),
        }
    }
}

#[derive(Debug)]
struct SimilaritySignature {
    spans: Vec<(u32, u32)>,
    total: usize,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
struct RenameCandidate {
    deletion: usize,
    addition: usize,
    score: u32,
    basename_equal: bool,
}

const CANDIDATES_PER_DESTINATION: usize = 4;
const MAX_RENAME_SCORE: u32 = 60_000;
const MIN_RENAME_SCORE: u32 = MAX_RENAME_SCORE / 2;
type RenameMatches = (Vec<(usize, usize)>, Vec<usize>);
type RawRenames = Vec<(Vec<u8>, Vec<u8>)>;

fn compare_rename_candidates(left: &RenameCandidate, right: &RenameCandidate) -> Ordering {
    right
        .score
        .cmp(&left.score)
        .then_with(|| right.basename_equal.cmp(&left.basename_equal))
}

fn record_if_better(slots: &mut Vec<RenameCandidate>, candidate: RenameCandidate) {
    if slots.len() < CANDIDATES_PER_DESTINATION {
        slots.push(candidate);
        return;
    }

    let mut worst = 0;
    for index in 1..slots.len() {
        if compare_rename_candidates(&slots[index], &slots[worst]) == Ordering::Greater {
            worst = index;
        }
    }
    if compare_rename_candidates(&slots[worst], &candidate) == Ordering::Greater {
        slots[worst] = candidate;
    }
}

fn select_rename_pairs(
    mut candidates: Vec<RenameCandidate>,
    deletion_count: usize,
    addition_count: usize,
) -> (Vec<(usize, usize)>, Vec<usize>) {
    // `sort_by` is stable, so score/name ties retain the destination-major
    // encounter order produced below, matching Git's stable matrix sort.
    candidates.sort_by(compare_rename_candidates);

    let mut used_deletions = vec![false; deletion_count];
    let mut used_additions = vec![false; addition_count];
    let mut renames = Vec::new();
    for candidate in candidates {
        if used_deletions[candidate.deletion] || used_additions[candidate.addition] {
            continue;
        }
        used_deletions[candidate.deletion] = true;
        used_additions[candidate.addition] = true;
        renames.push((candidate.deletion, candidate.addition));
    }
    let unmatched_additions = used_additions
        .into_iter()
        .enumerate()
        .filter_map(|(index, used)| (!used).then_some(index))
        .collect();
    (renames, unmatched_additions)
}

fn match_renames(
    repo: &gix::Repository,
    old_tree: gix::hash::ObjectId,
    new_tree: gix::hash::ObjectId,
    deletions: &[TreeEntry],
    additions: &[TreeEntry],
    ambiguous_rename_fallback: bool,
) -> Result<RenameMatches, AnyError> {
    let mut deletion_signatures: Vec<Option<SimilaritySignature>> =
        (0..deletions.len()).map(|_| None).collect();
    let mut addition_signatures: Vec<Option<SimilaritySignature>> =
        (0..additions.len()).map(|_| None).collect();
    let mut candidates =
        Vec::with_capacity(additions.len().saturating_mul(CANDIDATES_PER_DESTINATION));
    for (addition_index, addition) in additions.iter().enumerate() {
        let mut destination_candidates = Vec::with_capacity(CANDIDATES_PER_DESTINATION);
        for (deletion_index, deletion) in deletions.iter().enumerate() {
            if !same_git_type(deletion.mode, addition.mode) {
                continue;
            }
            let score = if deletion.oid == addition.oid {
                MAX_RENAME_SCORE
            } else {
                if deletion_signatures[deletion_index].is_none() {
                    deletion_signatures[deletion_index] =
                        Some(similarity_signature(repo, deletion)?);
                }
                if addition_signatures[addition_index].is_none() {
                    addition_signatures[addition_index] =
                        Some(similarity_signature(repo, addition)?);
                }
                signature_similarity(
                    deletion_signatures[deletion_index]
                        .as_ref()
                        .expect("signature initialized"),
                    addition_signatures[addition_index]
                        .as_ref()
                        .expect("signature initialized"),
                )
            };
            if score >= MIN_RENAME_SCORE {
                record_if_better(
                    &mut destination_candidates,
                    RenameCandidate {
                        deletion: deletion_index,
                        addition: addition_index,
                        score,
                        basename_equal: basename(&deletion.path) == basename(&addition.path),
                    },
                );
            }
        }
        candidates.extend(destination_candidates);
    }
    if ambiguous_rename_fallback && has_competing_rename_tie(&candidates) {
        return stock_rename_matches(repo, old_tree, new_tree, deletions, additions);
    }
    Ok(select_rename_pairs(
        candidates,
        deletions.len(),
        additions.len(),
    ))
}

fn has_competing_rename_tie(candidates: &[RenameCandidate]) -> bool {
    let mut ordered = candidates.to_vec();
    ordered.sort_by(compare_rename_candidates);
    ordered.windows(2).any(|pair| {
        compare_rename_candidates(&pair[0], &pair[1]) == Ordering::Equal
            && (pair[0].addition == pair[1].addition || pair[0].deletion == pair[1].deletion)
    })
}

fn stock_rename_matches(
    repo: &gix::Repository,
    old_tree: gix::hash::ObjectId,
    new_tree: gix::hash::ObjectId,
    deletions: &[TreeEntry],
    additions: &[TreeEntry],
) -> Result<RenameMatches, AnyError> {
    let mut command = Command::new("git");
    configure_stock_rename_command(
        &mut command,
        repo.path(),
        &old_tree.to_string(),
        &new_tree.to_string(),
    );
    let output = command.output()?;
    if !output.status.success() {
        return Err(format!(
            "stock Git rename fallback failed: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        )
        .into());
    }

    let deletion_by_path: HashMap<&[u8], usize> = deletions
        .iter()
        .enumerate()
        .map(|(index, entry)| (entry.path.as_slice(), index))
        .collect();
    let addition_by_path: HashMap<&[u8], usize> = additions
        .iter()
        .enumerate()
        .map(|(index, entry)| (entry.path.as_slice(), index))
        .collect();
    let mut renames = Vec::new();
    let mut used_additions = vec![false; additions.len()];
    for (old_path, new_path) in parse_raw_renames(&output.stdout)? {
        let (Some(&deletion), Some(&addition)) = (
            deletion_by_path.get(old_path.as_slice()),
            addition_by_path.get(new_path.as_slice()),
        ) else {
            continue;
        };
        renames.push((deletion, addition));
        used_additions[addition] = true;
    }
    let unmatched_additions = used_additions
        .into_iter()
        .enumerate()
        .filter_map(|(index, used)| (!used).then_some(index))
        .collect();
    Ok((renames, unmatched_additions))
}

fn configure_stock_rename_command(
    command: &mut Command,
    git_dir: &Path,
    old_tree: &str,
    new_tree: &str,
) {
    command.arg("--git-dir").arg(git_dir).args([
        "-c",
        "core.quotePath=true",
        "diff-tree",
        "-r",
        "--raw",
        "-z",
        "--no-commit-id",
        "--no-abbrev",
        "--diff-filter=R",
        "-M50%",
        old_tree,
        new_tree,
    ]);
    isolate_git_subprocess(command);
}

fn isolate_git_subprocess(command: &mut Command) {
    let null_device = if cfg!(windows) { "NUL" } else { "/dev/null" };
    command
        .env("GIT_CONFIG_GLOBAL", null_device)
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("GIT_CONFIG_SYSTEM", null_device)
        .env("GIT_NO_REPLACE_OBJECTS", "1")
        .env("GIT_TERMINAL_PROMPT", "0")
        .env("GIT_CONFIG_COUNT", "0")
        .env_remove("GIT_CONFIG_KEY_0")
        .env_remove("GIT_CONFIG_VALUE_0")
        .env_remove("GIT_CONFIG_PARAMETERS")
        .env_remove("GIT_DIFF_OPTS");
}

fn parse_raw_renames(output: &[u8]) -> Result<RawRenames, AnyError> {
    let fields: Vec<_> = output
        .split(|byte| *byte == 0)
        .filter(|field| !field.is_empty())
        .collect();
    let mut renames = Vec::new();
    let mut index = 0;
    while index < fields.len() {
        let header = fields[index];
        index += 1;
        let status = header
            .rsplit(|byte| *byte == b' ')
            .next()
            .ok_or("stock Git rename fallback emitted an empty raw header")?;
        if !status.starts_with(b"R") || index + 1 >= fields.len() {
            return Err("stock Git rename fallback emitted malformed raw output".into());
        }
        renames.push((fields[index].to_vec(), fields[index + 1].to_vec()));
        index += 2;
    }
    Ok(renames)
}

fn similarity_signature(
    repo: &gix::Repository,
    entry: &TreeEntry,
) -> Result<SimilaritySignature, AnyError> {
    let data = if u32::from(entry.mode.value()) == 0o160_000 {
        entry.oid.as_bytes().to_vec()
    } else {
        raw_blob(repo, entry.oid.as_bytes())?
    };
    let text = !data.iter().take(8000).any(|byte| *byte == 0);
    let mut spans = Vec::with_capacity(data.len() / 32 + 1);
    let mut accum1 = 0u32;
    let mut accum2 = 0u32;
    let mut length = 0u32;
    let mut index = 0usize;
    while index < data.len() {
        let value = data[index];
        if text && value == b'\r' && data.get(index + 1) == Some(&b'\n') {
            index += 1;
            continue;
        }
        let previous = accum1;
        accum1 = (accum1 << 7) ^ (accum2 >> 25);
        accum2 = (accum2 << 7) ^ (previous >> 25);
        accum1 = accum1.wrapping_add(u32::from(value));
        length += 1;
        if length >= 64 || value == b'\n' {
            spans.push((span_hash(accum1, accum2), length));
            accum1 = 0;
            accum2 = 0;
            length = 0;
        }
        index += 1;
    }
    if length != 0 {
        spans.push((span_hash(accum1, accum2), length));
    }
    spans.sort_unstable_by_key(|span| span.0);
    Ok(SimilaritySignature {
        total: spans.iter().map(|span| span.1 as usize).sum(),
        spans,
    })
}

fn span_hash(accum1: u32, accum2: u32) -> u32 {
    accum1.wrapping_add(accum2.wrapping_mul(0x61)) % 107_927
}

fn signature_similarity(left: &SimilaritySignature, right: &SimilaritySignature) -> u32 {
    let mut left_index = 0usize;
    let mut right_index = 0usize;
    let mut copied = 0usize;
    while left_index < left.spans.len() && right_index < right.spans.len() {
        match left.spans[left_index].0.cmp(&right.spans[right_index].0) {
            Ordering::Less => left_index += 1,
            Ordering::Greater => right_index += 1,
            Ordering::Equal => {
                let hash = left.spans[left_index].0;
                let mut left_bytes = 0usize;
                let mut right_bytes = 0usize;
                while left_index < left.spans.len() && left.spans[left_index].0 == hash {
                    left_bytes += left.spans[left_index].1 as usize;
                    left_index += 1;
                }
                while right_index < right.spans.len() && right.spans[right_index].0 == hash {
                    right_bytes += right.spans[right_index].1 as usize;
                    right_index += 1;
                }
                copied += left_bytes.min(right_bytes);
            }
        }
    }
    let maximum = left.total.max(right.total);
    if maximum == 0 {
        0
    } else {
        u32::try_from(copied.saturating_mul(MAX_RENAME_SCORE as usize) / maximum)
            .unwrap_or(MAX_RENAME_SCORE)
    }
}

fn basename(path: &[u8]) -> &[u8] {
    path.rsplit(|byte| *byte == b'/').next().unwrap_or(path)
}

#[expect(
    clippy::too_many_arguments,
    reason = "protocol emission needs explicit lifecycle state"
)]
fn emit_file(
    status: u8,
    old: Option<&TreeEntry>,
    new: &TreeEntry,
    repo: &gix::Repository,
    diff_cache: &mut gix::diff::blob::Platform,
    output: &mut impl Write,
    batch_id: u64,
    commit_oid: &[u8],
    counters: &mut Counters,
) -> Result<(), AnyError> {
    let old_mode = old.map_or(0, |entry| u32::from(entry.mode.value()));
    let new_mode = u32::from(new.mode.value());
    let old_object_id = old.map_or_else(|| new.oid.kind().null(), |entry| entry.oid);
    let old_oid = old_object_id.as_bytes();
    let new_oid = new.oid.as_bytes();
    let old_path = old.map_or(new.path.as_slice(), |entry| entry.path.as_slice());
    let new_path = new.path.as_slice();

    let content_changed = old_oid != new_oid;
    let mut hunks = Vec::new();
    let mut binary = false;
    if old_mode == 0o160_000 || new_mode == 0o160_000 {
        return Err("submodule patch projection is not implemented by the gix candidate".into());
    }
    if content_changed && new_mode != 0o160_000 && old_mode != 0o160_000 {
        let force_text = diff_attribute_forces_text(diff_cache, repo, new_path)?;
        diff_cache.set_resource(
            old_object_id,
            old.map_or(new.mode.kind(), |entry| entry.mode.kind()),
            old_path.as_bstr(),
            gix::diff::blob::ResourceKind::OldOrSource,
            &repo.objects,
        )?;
        diff_cache.set_resource(
            new.oid,
            new.mode.kind(),
            new_path.as_bstr(),
            gix::diff::blob::ResourceKind::NewOrDestination,
            &repo.objects,
        )?;
        let prepared = diff_cache.prepare_diff()?;
        match prepared.operation {
            Operation::SourceOrDestinationIsBinary if force_text => {
                let old_data = raw_blob(repo, old_oid)?;
                let new_data = raw_blob(repo, new_oid)?;
                hunks = compute_hunks(&old_data, &new_data)?;
            }
            Operation::SourceOrDestinationIsBinary => binary = true,
            Operation::ExternalCommand { .. } => {
                return Err("external diff command escaped --no-ext-diff semantics".into());
            }
            Operation::InternalDiff { .. } => {
                if prepared.old_or_new_is_derived {
                    return Err("textconv output is unsupported by the gix candidate".into());
                }
                let old_data = raw_blob(repo, old_oid)?;
                let new_data = raw_blob(repo, new_oid)?;
                hunks = compute_hunks(&old_data, &new_data)?;
            }
        }
    }

    let mut payload = Vec::new();
    append_bytes(&mut payload, commit_oid)?;
    payload.push(status);
    payload.extend_from_slice(&old_mode.to_be_bytes());
    payload.extend_from_slice(&new_mode.to_be_bytes());
    append_bytes(&mut payload, old_oid)?;
    append_bytes(&mut payload, new_oid)?;
    append_bytes(&mut payload, old_path)?;
    append_bytes(&mut payload, new_path)?;
    payload.push(u8::from(binary));
    write_frame(output, FBEG, batch_id, &payload)?;
    counters.files += 1;
    for (position, added, missing) in hunks {
        write_hunk(
            output, batch_id, commit_oid, new_path, position, &added, missing,
        )?;
        counters.hunks += 1;
    }
    write_frame(output, FEND, batch_id, &[])?;
    Ok(())
}

fn diff_attribute_forces_text(
    diff_cache: &mut gix::diff::blob::Platform,
    repo: &gix::Repository,
    path: &[u8],
) -> Result<bool, AnyError> {
    let mut matches = diff_cache.attr_stack.selected_attribute_matches(["diff"]);
    diff_cache
        .attr_stack
        .at_entry(path.as_bstr(), None, &repo.objects)?
        .matching_attributes(&mut matches);
    let state = matches
        .iter_selected()
        .next()
        .ok_or("diff attribute selection disappeared")?
        .assignment
        .state;
    Ok(state.is_set() && state.as_bstr().is_none())
}

fn raw_blob(repo: &gix::Repository, oid: &[u8]) -> Result<Vec<u8>, AnyError> {
    if oid.iter().all(|byte| *byte == 0) {
        return Ok(Vec::new());
    }
    let mut blob = repo.find_blob(gix::hash::ObjectId::try_from(oid)?)?;
    Ok(blob.take_data())
}

fn compute_hunks(old_data: &[u8], new_data: &[u8]) -> Result<Vec<(u64, Vec<u8>, bool)>, AnyError> {
    let (old_data, new_data) = trim_large_common_tail(old_data, new_data);
    let mut options = git2::DiffOptions::new();
    options
        .context_lines(0)
        .interhunk_lines(0)
        .indent_heuristic(true)
        .force_text(true);
    let patch = git2::Patch::from_buffers(old_data, None, new_data, None, Some(&mut options))?;
    let mut hunks = Vec::with_capacity(patch.num_hunks());
    for hunk_index in 0..patch.num_hunks() {
        let (hunk, line_count) = patch.hunk(hunk_index)?;
        let mut added = Vec::new();
        let mut missing = false;
        for line_index in 0..line_count {
            let line = patch.line_in_hunk(hunk_index, line_index)?;
            if line.origin() == '+' {
                let content = line.content();
                let new_size = added
                    .len()
                    .checked_add(content.len())
                    .ok_or("hunk byte length overflow")?;
                if new_size > wire::MAX_RECORD {
                    return Err("hunk exceeds 256 MiB limit".into());
                }
                added.extend_from_slice(content);
                missing = !content.ends_with(b"\n");
            }
        }
        hunks.push((u64::from(hunk.new_start()), added, missing));
    }
    Ok(hunks)
}

fn trim_large_common_tail<'a>(old_data: &'a [u8], new_data: &'a [u8]) -> (&'a [u8], &'a [u8]) {
    const BLOCK: usize = 1024;
    let smaller = old_data.len().min(new_data.len());
    if smaller < BLOCK {
        return (old_data, new_data);
    }
    let mut trimmed = 0usize;
    let mut old_end = old_data.len();
    let mut new_end = new_data.len();
    while BLOCK + trimmed <= smaller
        && old_data[old_end - BLOCK..old_end] == new_data[new_end - BLOCK..new_end]
    {
        trimmed += BLOCK;
        old_end -= BLOCK;
        new_end -= BLOCK;
    }
    let retained = old_data[old_end..old_end + trimmed]
        .iter()
        .position(|byte| *byte == b'\n')
        .map_or(trimmed, |position| position + 1);
    let removed = trimmed - retained;
    (
        &old_data[..old_data.len() - removed],
        &new_data[..new_data.len() - removed],
    )
}

fn same_git_type(old: gix::objs::tree::EntryMode, new: gix::objs::tree::EntryMode) -> bool {
    fn class(mode: gix::objs::tree::EntryMode) -> u8 {
        if mode.is_blob() {
            1
        } else if mode.is_link() {
            2
        } else if mode.is_tree() {
            3
        } else {
            4
        }
    }
    class(old) == class(new)
}

#[cfg(test)]
fn lines_with_terminators(data: &[u8]) -> Vec<&[u8]> {
    let mut lines = Vec::new();
    let mut start = 0;
    for (index, byte) in data.iter().enumerate() {
        if *byte == b'\n' {
            lines.push(&data[start..=index]);
            start = index + 1;
        }
    }
    if start < data.len() {
        lines.push(&data[start..]);
    }
    lines
}

fn normalize_message(message: &[u8]) -> Vec<u8> {
    let lines: Vec<&[u8]> = message.split(|byte| *byte == b'\n').collect();
    let mut title = Vec::new();
    let mut body = Vec::new();
    let mut in_title = true;
    let mut skip_body = false;
    let mut pending_empty = 0usize;
    let mut title_indent = Vec::new();
    for line in lines {
        let expanded = expand_tabs(line);
        let line = expanded.as_ref();
        let trimmed = trim_space(line);
        if in_title {
            if trimmed.is_empty() {
                skip_body = title.is_empty();
                in_title = false;
                continue;
            }
            if title.is_empty() {
                title_indent.extend_from_slice(&line[..leading_space_len(line)]);
            } else {
                title.push(b' ');
            }
            title.extend_from_slice(trimmed);
            continue;
        }
        if skip_body {
            continue;
        }
        let mut line = trim_right_space(line);
        if line.starts_with(&title_indent) {
            line = &line[title_indent.len()..];
        }
        if line.is_empty() {
            pending_empty += 1;
            continue;
        }
        if !body.is_empty() {
            body.push(b'\n');
            if pending_empty > 0 {
                body.push(b'\n');
            }
        }
        pending_empty = 0;
        body.extend_from_slice(line);
    }
    if title.is_empty() {
        return Vec::new();
    }
    if !body.is_empty() {
        title.extend_from_slice(b"\n\n");
        title.extend_from_slice(&body);
    }
    title
}

fn expand_tabs(bytes: &[u8]) -> Cow<'_, [u8]> {
    if !bytes.contains(&b'\t') {
        return Cow::Borrowed(bytes);
    }
    let mut expanded = Vec::with_capacity(bytes.len());
    let mut column = 0usize;
    for byte in bytes {
        if *byte == b'\t' {
            let spaces = 8 - column % 8;
            expanded.resize(expanded.len() + spaces, b' ');
            column += spaces;
        } else {
            expanded.push(*byte);
            column += 1;
        }
    }
    Cow::Owned(expanded)
}

fn trim_space(bytes: &[u8]) -> &[u8] {
    if let Ok(text) = std::str::from_utf8(bytes) {
        text.trim().as_bytes()
    } else {
        let start = bytes
            .iter()
            .position(|byte| !byte.is_ascii_whitespace())
            .unwrap_or(bytes.len());
        let end = bytes
            .iter()
            .rposition(|byte| !byte.is_ascii_whitespace())
            .map_or(start, |index| index + 1);
        &bytes[start..end]
    }
}

fn trim_right_space(bytes: &[u8]) -> &[u8] {
    if let Ok(text) = std::str::from_utf8(bytes) {
        text.trim_end().as_bytes()
    } else {
        let end = bytes
            .iter()
            .rposition(|byte| !byte.is_ascii_whitespace())
            .map_or(0, |index| index + 1);
        &bytes[..end]
    }
}

fn leading_space_len(bytes: &[u8]) -> usize {
    if let Ok(text) = std::str::from_utf8(bytes) {
        text.char_indices()
            .find_map(|(index, character)| (!character.is_whitespace()).then_some(index))
            .unwrap_or(bytes.len())
    } else {
        bytes
            .iter()
            .take_while(|byte| byte.is_ascii_whitespace())
            .count()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn run_git(repo: Option<&Path>, args: &[&str]) -> Vec<u8> {
        let mut command = Command::new("git");
        if let Some(repo) = repo {
            command.arg("-C").arg(repo);
        }
        command.args(args);
        isolate_git_subprocess(&mut command);
        let output = command.output().expect("git test fixture command starts");
        assert!(
            output.status.success(),
            "git {:?} failed: {}",
            args,
            String::from_utf8_lossy(&output.stderr)
        );
        output.stdout
    }

    fn rename_candidate(
        deletion: usize,
        addition: usize,
        score: u32,
        basename_equal: bool,
    ) -> RenameCandidate {
        RenameCandidate {
            deletion,
            addition,
            score,
            basename_equal,
        }
    }

    #[test]
    fn accepts_direct_and_git_subcommand_argument_forms() {
        let repo = PathBuf::from("/tmp/repo");
        assert_eq!(
            Args::parse([OsString::from("/tmp/repo")]).unwrap(),
            Args {
                repo: repo.clone(),
                ambiguous_rename_fallback: false,
            }
        );
        assert_eq!(
            Args::parse([OsString::from("--repo"), OsString::from("/tmp/repo")]).unwrap(),
            Args {
                repo: repo.clone(),
                ambiguous_rename_fallback: false,
            }
        );
        assert_eq!(
            Args::parse([
                OsString::from("-C"),
                OsString::from("/tmp/repo"),
                OsString::from("betterleaks--diff-engine"),
                OsString::from("--protocol=1"),
            ])
            .unwrap(),
            Args {
                repo,
                ambiguous_rename_fallback: false,
            }
        );
    }

    #[test]
    fn ambiguous_rename_fallback_requires_an_explicit_unique_flag() {
        let expected = Args {
            repo: PathBuf::from("/tmp/repo"),
            ambiguous_rename_fallback: true,
        };
        assert_eq!(
            Args::parse([
                OsString::from("--repo"),
                OsString::from("/tmp/repo"),
                OsString::from("--ambiguous-rename-fallback"),
            ])
            .unwrap(),
            expected
        );
        assert!(
            Args::parse([
                OsString::from("/tmp/repo"),
                OsString::from("--ambiguous-rename-fallback"),
                OsString::from("--ambiguous-rename-fallback"),
            ])
            .is_err()
        );
    }

    #[test]
    fn message_projection_matches_pretty_medium_shape() {
        assert_eq!(
            normalize_message(b"title\n\nbody one\n\n\nbody two\n"),
            b"title\n\nbody one\n\nbody two"
        );
        assert_eq!(normalize_message(b"one\ntwo\n"), b"one two");
        assert_eq!(normalize_message(b"\nbody without title\n"), b"");
        assert_eq!(normalize_message(b" title \n\n body  \n"), b"title\n\nbody");
        assert_eq!(
            normalize_message(b"title\n\nConflicts:\n\tfile.txt\n"),
            b"title\n\nConflicts:\n        file.txt"
        );
        assert_eq!(normalize_message("title\u{85}\n".as_bytes()), b"title");
    }

    #[test]
    fn preserves_line_terminators_and_missing_final_line() {
        assert_eq!(
            lines_with_terminators(b"a\r\nb\nlast"),
            vec![b"a\r\n".as_slice(), b"b\n", b"last"]
        );
        assert!(lines_with_terminators(b"").is_empty());
    }

    #[test]
    fn trims_block_aligned_common_tail_but_retains_through_the_next_newline() {
        let mut old = b"old prefix\n".to_vec();
        let mut new = b"new prefix\n".to_vec();
        let mut tail = vec![b'x'; 2500];
        tail[700] = b'\n';
        old.extend_from_slice(&tail);
        new.extend_from_slice(&tail);

        let (old_trimmed, new_trimmed) = trim_large_common_tail(&old, &new);
        assert_eq!(old_trimmed.len(), new_trimmed.len());
        assert!(old_trimmed.len() < old.len());
        assert_eq!(old_trimmed.last(), Some(&b'\n'));
        assert_eq!(&old_trimmed[..11], b"old prefix\n");
        assert_eq!(&new_trimmed[..11], b"new prefix\n");
    }

    #[test]
    fn rename_admission_keeps_only_four_and_does_not_replace_an_equal_tie() {
        let mut slots = Vec::new();
        for deletion in 0..5 {
            record_if_better(&mut slots, rename_candidate(deletion, 0, 53, false));
        }

        assert_eq!(
            slots,
            vec![
                rename_candidate(0, 0, 53, false),
                rename_candidate(1, 0, 53, false),
                rename_candidate(2, 0, 53, false),
                rename_candidate(3, 0, 53, false),
            ]
        );
    }

    #[test]
    fn rename_admission_replaces_the_worst_candidate_only_when_strictly_better() {
        let mut slots = vec![
            rename_candidate(0, 0, 80, false),
            rename_candidate(1, 0, 70, false),
            rename_candidate(2, 0, 60, false),
            rename_candidate(3, 0, 50, false),
        ];

        record_if_better(&mut slots, rename_candidate(4, 0, 50, false));
        assert!(!slots.iter().any(|candidate| candidate.deletion == 4));

        record_if_better(&mut slots, rename_candidate(5, 0, 50, true));
        assert!(slots.iter().any(|candidate| candidate.deletion == 5));
        assert!(!slots.iter().any(|candidate| candidate.deletion == 3));
    }

    #[test]
    fn rename_selection_preserves_destination_major_order_for_exact_ties() {
        let candidates = vec![
            rename_candidate(0, 0, 53, false),
            rename_candidate(1, 0, 53, false),
            rename_candidate(2, 1, 53, false),
            rename_candidate(3, 1, 53, false),
        ];

        let (renames, unmatched) = select_rename_pairs(candidates, 4, 2);

        assert_eq!(renames, vec![(0, 0), (2, 1)]);
        assert!(unmatched.is_empty());
    }

    #[test]
    fn rename_selection_is_greedy_and_uses_each_source_and_destination_once() {
        let candidates = vec![
            rename_candidate(0, 0, 90, false),
            rename_candidate(0, 1, 80, false),
            rename_candidate(1, 0, 70, false),
            rename_candidate(2, 2, 60, false),
        ];

        let (renames, unmatched) = select_rename_pairs(candidates, 3, 4);

        assert_eq!(renames, vec![(0, 0), (2, 2)]);
        assert_eq!(unmatched, vec![1, 3]);
    }

    #[test]
    fn competing_rename_tie_requires_a_shared_source_or_destination() {
        assert!(has_competing_rename_tie(&[
            rename_candidate(0, 0, 53, false),
            rename_candidate(1, 0, 53, false),
        ]));
        assert!(has_competing_rename_tie(&[
            rename_candidate(0, 0, 53, false),
            rename_candidate(0, 1, 53, false),
        ]));
        assert!(!has_competing_rename_tie(&[
            rename_candidate(0, 0, 53, false),
            rename_candidate(1, 1, 53, false),
        ]));
        assert!(!has_competing_rename_tie(&[
            rename_candidate(0, 0, 54, false),
            rename_candidate(1, 0, 53, false),
        ]));
    }

    #[test]
    fn parses_nul_terminated_raw_rename_records() {
        let output = concat!(
            ":100644 100644 aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa ",
            "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb R053\0old name\0new name\0",
            ":100644 100644 cccccccccccccccccccccccccccccccccccccccc ",
            "dddddddddddddddddddddddddddddddddddddddd R100\0old\0new\0",
        );

        assert_eq!(
            parse_raw_renames(output.as_bytes()).unwrap(),
            vec![
                (b"old name".to_vec(), b"new name".to_vec()),
                (b"old".to_vec(), b"new".to_vec()),
            ]
        );
        assert!(parse_raw_renames(b"malformed\0old\0").is_err());
    }

    #[test]
    fn stock_rename_fallback_ignores_hostile_global_rename_config() {
        let fixture = tempfile::tempdir().unwrap();
        let repo = fixture.path();
        run_git(None, &["init", repo.to_str().unwrap()]);
        run_git(Some(repo), &["config", "user.name", "Fixture User"]);
        run_git(
            Some(repo),
            &["config", "user.email", "fixture@example.test"],
        );

        std::fs::write(
            repo.join("alpha-old.yml"),
            b"kind: feature\nname: alpha\nowner: group\nrollout: old\n",
        )
        .unwrap();
        std::fs::write(
            repo.join("beta-old.yml"),
            b"kind: feature\nname: beta\nowner: group\nrollout: old\n",
        )
        .unwrap();
        run_git(Some(repo), &["add", "."]);
        run_git(Some(repo), &["commit", "-m", "old tree"]);
        let old_tree =
            String::from_utf8(run_git(Some(repo), &["rev-parse", "HEAD^{tree}"])).unwrap();

        std::fs::remove_file(repo.join("alpha-old.yml")).unwrap();
        std::fs::remove_file(repo.join("beta-old.yml")).unwrap();
        std::fs::write(
            repo.join("alpha-new.yml"),
            b"kind: feature\nname: alpha\nowner: group\nrollout: new\n",
        )
        .unwrap();
        std::fs::write(
            repo.join("beta-new.yml"),
            b"kind: feature\nname: beta\nowner: group\nrollout: new\n",
        )
        .unwrap();
        run_git(Some(repo), &["add", "-A"]);
        run_git(Some(repo), &["commit", "-m", "new tree"]);
        let new_tree =
            String::from_utf8(run_git(Some(repo), &["rev-parse", "HEAD^{tree}"])).unwrap();

        let hostile = repo.join("hostile.gitconfig");
        std::fs::write(&hostile, b"[diff]\n\trenameLimit = 1\n\trenames = false\n").unwrap();
        let mut command = Command::new("git");
        command
            .env("GIT_CONFIG_GLOBAL", &hostile)
            .env("GIT_CONFIG_COUNT", "1")
            .env("GIT_CONFIG_KEY_0", "diff.renameLimit")
            .env("GIT_CONFIG_VALUE_0", "1")
            .env("GIT_DIFF_OPTS", "--unified=99");
        configure_stock_rename_command(
            &mut command,
            &repo.join(".git"),
            old_tree.trim(),
            new_tree.trim(),
        );
        let output = command.output().unwrap();
        assert!(
            output.status.success(),
            "isolated rename fallback failed: {}",
            String::from_utf8_lossy(&output.stderr)
        );
        let mut renames = parse_raw_renames(&output.stdout).unwrap();
        renames.sort();
        assert_eq!(
            renames,
            vec![
                (b"alpha-old.yml".to_vec(), b"alpha-new.yml".to_vec()),
                (b"beta-old.yml".to_vec(), b"beta-new.yml".to_vec()),
            ]
        );
    }
}
