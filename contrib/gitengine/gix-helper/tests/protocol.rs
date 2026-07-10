use std::collections::BTreeMap;
use std::fs;
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::process::{Child, ChildStdin, ChildStdout, Command, Stdio};

const BGIN: [u8; 4] = *b"BGIN";
const CMIT: [u8; 4] = *b"CMIT";
const FBEG: [u8; 4] = *b"FBEG";
const HUNK: [u8; 4] = *b"HUNK";
const HBGN: [u8; 4] = *b"HBGN";
const HADD: [u8; 4] = *b"HADD";
const HEND: [u8; 4] = *b"HEND";
const FEND: [u8; 4] = *b"FEND";
const CEND: [u8; 4] = *b"CEND";
const BEND: [u8; 4] = *b"BEND";
const ERRO: [u8; 4] = *b"ERRO";
const QUIT: [u8; 4] = *b"QUIT";

#[derive(Debug)]
struct Frame {
    tag: [u8; 4],
    batch: u64,
    payload: Vec<u8>,
}

struct Helper {
    child: Child,
    input: ChildStdin,
    output: ChildStdout,
    oid_len: usize,
}

impl Helper {
    fn start(repo: &Path) -> Self {
        let mut child = Command::new(env!("CARGO_BIN_EXE_betterleaks-gix-helper"))
            .arg(repo)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped())
            .stderr(Stdio::piped())
            .spawn()
            .unwrap();
        let mut output = child.stdout.take().unwrap();
        let hello = read_frame(&mut output).unwrap();
        assert_eq!(hello.tag, *b"HELO");
        assert_eq!(hello.batch, 0);
        assert_eq!(
            u16::from_be_bytes(hello.payload[..2].try_into().unwrap()),
            1
        );
        let oid_len = usize::from(u16::from_be_bytes(hello.payload[2..4].try_into().unwrap()));
        assert_eq!(&hello.payload[4..], &[0, 0]);
        let input = child.stdin.take().unwrap();
        Self {
            child,
            input,
            output,
            oid_len,
        }
    }

    fn scan(&mut self, batch: u64, commits: &[Vec<u8>]) -> Vec<Frame> {
        let mut payload = u32::try_from(commits.len()).unwrap().to_be_bytes().to_vec();
        for oid in commits {
            append_bytes(&mut payload, oid);
        }
        write_frame(&mut self.input, BGIN, batch, &payload).unwrap();
        self.input.flush().unwrap();
        let mut frames = Vec::new();
        loop {
            let frame = read_frame(&mut self.output).unwrap();
            let done = frame.tag == BEND || frame.tag == ERRO;
            frames.push(frame);
            if done {
                break;
            }
        }
        frames
    }

    fn finish(mut self) {
        write_frame(&mut self.input, QUIT, 0, &[]).unwrap();
        drop(self.input);
        assert!(self.child.wait().unwrap().success());
    }
}

#[derive(Debug)]
struct FileEvent {
    commit: Vec<u8>,
    status: u8,
    old_mode: u32,
    new_mode: u32,
    old_oid: Vec<u8>,
    new_oid: Vec<u8>,
    old_path: Vec<u8>,
    new_path: Vec<u8>,
    binary: bool,
}

#[derive(Debug)]
struct HunkEvent {
    commit: Vec<u8>,
    path: Vec<u8>,
    position: u64,
    added: Vec<u8>,
    missing: bool,
}

#[test]
fn live_repository_contract_and_sequential_reset() {
    let temp = tempfile::tempdir().unwrap();
    init_repo(temp.path(), "sha1");
    let root = commit_file(temp.path(), "root.txt", b"one\n", "root");
    let empty = commit(temp.path(), "empty", true);
    fs::write(temp.path().join("root.txt"), b"one\ntwo").unwrap();
    let no_final = commit(temp.path(), "no final", false);
    set_executable(&temp.path().join("root.txt"));
    let mode = commit(temp.path(), "mode", false);
    git(temp.path(), &["mv", "root.txt", "renamed.txt"]);
    let rename = commit(temp.path(), "rename", false);
    fs::write(temp.path().join("binary.dat"), b"a\0b\n").unwrap();
    let binary = commit(temp.path(), "binary", false);
    let weird_path = raw_path(temp.path());
    fs::write(temp.path().join(&weird_path), b"weird\n").unwrap();
    let weird = commit(temp.path(), "weird", false);

    let commits = [&root, &empty, &no_final, &mode, &rename, &binary, &weird]
        .into_iter()
        .map(|hex| decode_hex(hex))
        .collect::<Vec<_>>();
    let mut helper = Helper::start(temp.path());
    assert_eq!(helper.oid_len, 20);
    let frames = helper.scan(7, &commits);
    assert_eq!(frames.first().unwrap().tag, BGIN);
    assert_eq!(frames.last().unwrap().tag, BEND);
    assert!(frames.iter().all(|frame| frame.batch == 7));
    let commit_count = frames.iter().filter(|frame| frame.tag == CMIT).count();
    assert_eq!(commit_count, commits.len());

    let mut files: BTreeMap<Vec<u8>, Vec<FileEvent>> = BTreeMap::new();
    let mut hunks: BTreeMap<Vec<u8>, Vec<HunkEvent>> = BTreeMap::new();
    let mut current_file: Option<Vec<u8>> = None;
    for frame in &frames {
        match frame.tag {
            FBEG => {
                let event = decode_file(&frame.payload);
                current_file = Some(event.new_path.clone());
                files.entry(event.commit.clone()).or_default().push(event);
            }
            HUNK => {
                let event = decode_hunk(&frame.payload);
                assert_eq!(current_file.as_deref(), Some(event.path.as_slice()));
                hunks.entry(event.commit.clone()).or_default().push(event);
            }
            HBGN | HADD | HEND => panic!("small fixture unexpectedly used continuation"),
            FEND => current_file = None,
            _ => {}
        }
    }

    let by = |hex: &str| decode_hex(hex);
    assert_eq!(files[&by(&root)][0].status, b'A');
    assert!(!files.contains_key(&by(&empty)));
    assert_eq!(files[&by(&no_final)][0].status, b'M');
    let no_final_hunk = &hunks[&by(&no_final)][0];
    assert_eq!(no_final_hunk.position, 2);
    assert_eq!(no_final_hunk.added, b"two");
    assert!(no_final_hunk.missing);
    let mode_file = &files[&by(&mode)][0];
    assert_eq!(
        (mode_file.old_mode, mode_file.new_mode),
        (0o100_644, 0o100_755)
    );
    assert!(!hunks.contains_key(&by(&mode)));
    let rename_file = &files[&by(&rename)][0];
    assert_eq!(rename_file.status, b'R');
    assert_eq!(rename_file.old_path, b"root.txt");
    assert_eq!(rename_file.new_path, b"renamed.txt");
    assert!(!hunks.contains_key(&by(&rename)));
    let binary_file = files[&by(&binary)]
        .iter()
        .find(|file| file.new_path == b"binary.dat")
        .unwrap();
    assert!(binary_file.binary);
    assert!(!hunks.contains_key(&by(&binary)));
    assert!(
        files[&by(&weird)]
            .iter()
            .any(|file| file.new_path == path_bytes(&weird_path))
    );
    for events in files.values() {
        for event in events {
            assert_eq!(event.commit.len(), 20);
            assert_eq!(event.old_oid.len(), 20);
            assert_eq!(event.new_oid.len(), 20);
        }
    }

    let replay = helper.scan(8, &[decode_hex(&root)]);
    assert_eq!(replay.iter().filter(|frame| frame.tag == CMIT).count(), 1);
    assert_eq!(replay.iter().filter(|frame| frame.tag == FBEG).count(), 1);
    assert!(replay.iter().all(|frame| frame.batch == 8));
    helper.finish();
}

#[test]
fn sha256_root_commit_uses_thirty_two_byte_oids() {
    let temp = tempfile::tempdir().unwrap();
    init_repo(temp.path(), "sha256");
    let root = commit_file(temp.path(), "f", b"x\n", "root");
    let mut helper = Helper::start(temp.path());
    assert_eq!(helper.oid_len, 32);
    let frames = helper.scan(1, &[decode_hex(&root)]);
    let file = frames.iter().find(|frame| frame.tag == FBEG).unwrap();
    let file = decode_file(&file.payload);
    assert_eq!(file.commit.len(), 32);
    assert_eq!(file.old_oid, vec![0; 32]);
    assert_eq!(file.new_oid.len(), 32);
    helper.finish();
}

#[test]
fn merge_commit_emits_metadata_without_a_diff() {
    let temp = tempfile::tempdir().unwrap();
    init_repo(temp.path(), "sha1");
    commit_file(temp.path(), "root", b"root\n", "root");
    let main = git_output(temp.path(), &["symbolic-ref", "--short", "HEAD"])
        .trim()
        .to_owned();
    git(temp.path(), &["checkout", "--quiet", "-b", "side"]);
    commit_file(temp.path(), "side", b"side\n", "side");
    git(temp.path(), &["checkout", "--quiet", &main]);
    commit_file(temp.path(), "main", b"main\n", "main");
    git(
        temp.path(),
        &["merge", "--quiet", "--no-ff", "side", "-m", "merge"],
    );
    let merge = git_output(temp.path(), &["rev-parse", "HEAD"]);

    let mut helper = Helper::start(temp.path());
    let frames = helper.scan(1, &[decode_hex(merge.trim())]);
    assert_eq!(
        frames.iter().map(|frame| frame.tag).collect::<Vec<_>>(),
        vec![BGIN, CMIT, CEND, BEND]
    );
    helper.finish();
}

#[test]
fn invalid_and_noncommit_oids_are_terminal_before_records() {
    let temp = tempfile::tempdir().unwrap();
    init_repo(temp.path(), "sha1");
    let root = commit_file(temp.path(), "f", b"x\n", "root");
    let blob = git_output(temp.path(), &["rev-parse", "HEAD:f"]);
    for oid in [vec![0x55; 20], decode_hex(blob.trim())] {
        let mut helper = Helper::start(temp.path());
        let frames = helper.scan(3, &[oid]);
        assert_eq!(frames.len(), 1);
        assert_eq!(frames[0].tag, ERRO);
        assert!(
            !frames
                .iter()
                .any(|frame| matches!(frame.tag, CMIT | FBEG | HUNK))
        );
        drop(helper.input);
        assert!(!helper.child.wait().unwrap().success());
    }
    assert_eq!(decode_hex(&root).len(), 20);
}

#[test]
fn textconv_is_a_clean_pre_hello_unsupported_error() {
    let temp = tempfile::tempdir().unwrap();
    init_repo(temp.path(), "sha1");
    git(temp.path(), &["config", "diff.custom.textconv", "cat"]);
    let mut child = Command::new(env!("CARGO_BIN_EXE_betterleaks-gix-helper"))
        .arg(temp.path())
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    let frame = read_frame(child.stdout.as_mut().unwrap()).unwrap();
    assert_eq!(frame.tag, ERRO);
    assert_eq!(frame.batch, 0);
    assert_eq!(frame.payload[0], 5);
    assert!(child.wait().unwrap().success());
}

#[test]
fn mailmap_matches_canonical_git_author_projection() {
    let temp = tempfile::tempdir().unwrap();
    init_repo(temp.path(), "sha1");
    let root = commit_file(temp.path(), "f", b"x\n", "root");
    fs::write(
        temp.path().join(".mailmap"),
        b"Mapped Author <mapped@example.com> <TEST@EXAMPLE.COM>\n",
    )
    .unwrap();
    let canonical = git_output(
        temp.path(),
        &["log", "-1", "--use-mailmap", "--format=%aN <%aE>"],
    );
    assert_eq!(canonical.trim(), "Mapped Author <mapped@example.com>");

    let mut helper = Helper::start(temp.path());
    let frames = helper.scan(1, &[decode_hex(&root)]);
    let commit = frames.iter().find(|frame| frame.tag == CMIT).unwrap();
    let mut decoder = Decoder {
        bytes: &commit.payload,
        at: 0,
    };
    let _oid = decoder.bytes();
    let _message = decoder.bytes();
    assert_eq!(decoder.bytes(), b"Mapped Author");
    assert_eq!(decoder.bytes(), b"mapped@example.com");
    helper.finish();
}

#[test]
fn missing_author_matches_stock_git_projection() {
    let temp = tempfile::tempdir().unwrap();
    init_repo(temp.path(), "sha1");
    commit(temp.path(), "parent", true);
    let oid = commit_without_author(temp.path());

    let mut helper = Helper::start(temp.path());
    let frames = helper.scan(1, &[decode_hex(&oid)]);
    assert_eq!(
        frames.iter().map(|frame| frame.tag).collect::<Vec<_>>(),
        vec![BGIN, CMIT, CEND, BEND]
    );
    let mut decoder = Decoder {
        bytes: &frames[1].payload,
        at: 0,
    };
    assert_eq!(decoder.bytes(), decode_hex(&oid));
    assert_eq!(decoder.bytes(), b"missing author");
    assert!(decoder.bytes().is_empty());
    assert!(decoder.bytes().is_empty());
    assert_eq!(decoder.u64(), 0);
    assert_eq!(decoder.u32(), 0);
    assert_eq!(decoder.byte(), 0);
    assert_eq!(decoder.byte(), 0);
    assert_eq!(decoder.at, frames[1].payload.len());
    helper.finish();
}

#[test]
fn encoded_commit_is_terminal_after_hello_without_records() {
    let temp = tempfile::tempdir().unwrap();
    init_repo(temp.path(), "sha1");
    git(
        temp.path(),
        &["config", "i18n.commitEncoding", "ISO-8859-1"],
    );
    let root = commit_file(temp.path(), "f", b"x\n", "encoded");
    let mut helper = Helper::start(temp.path());
    let frames = helper.scan(1, &[decode_hex(&root)]);
    assert_eq!(frames.len(), 1);
    assert_eq!(frames[0].tag, ERRO);
    assert_eq!(frames[0].payload[0], 2);
    assert!(
        !frames
            .iter()
            .any(|frame| matches!(frame.tag, CMIT | FBEG | HUNK))
    );
    drop(helper.input);
    assert!(!helper.child.wait().unwrap().success());
}

#[test]
fn large_added_line_uses_hadd_continuations() {
    let temp = tempfile::tempdir().unwrap();
    init_repo(temp.path(), "sha1");
    let data = vec![b'x'; 17 << 20];
    let root = commit_file(temp.path(), "large", &data, "large");
    let mut helper = Helper::start(temp.path());
    let frames = helper.scan(1, &[decode_hex(&root)]);
    let begin = frames.iter().position(|frame| frame.tag == HBGN).unwrap();
    let end = frames.iter().position(|frame| frame.tag == HEND).unwrap();
    assert!(
        frames[begin + 1..end]
            .iter()
            .all(|frame| frame.tag == HADD && !frame.payload.is_empty())
    );
    assert_eq!(
        frames[begin + 1..end]
            .iter()
            .map(|frame| frame.payload.len())
            .sum::<usize>(),
        data.len()
    );
    assert_eq!(frames[end].payload, [1]);
    helper.finish();
}

fn init_repo(path: &Path, format: &str) {
    git(
        path,
        &["init", "--quiet", &format!("--object-format={format}")],
    );
    git(path, &["config", "user.name", "Test User"]);
    git(path, &["config", "user.email", "test@example.com"]);
    git(path, &["config", "core.fileMode", "true"]);
}

fn commit_file(path: &Path, name: &str, contents: &[u8], message: &str) -> String {
    fs::write(path.join(name), contents).unwrap();
    commit(path, message, false)
}

fn commit(path: &Path, message: &str, allow_empty: bool) -> String {
    git(path, &["add", "-A"]);
    let mut args = vec!["commit", "--quiet", "-m", message];
    if allow_empty {
        args.push("--allow-empty");
    }
    git(path, &args);
    git_output(path, &["rev-parse", "HEAD"]).trim().to_owned()
}

fn commit_without_author(path: &Path) -> String {
    let tree = git_output(path, &["write-tree"]);
    let parent = git_output(path, &["rev-parse", "HEAD"]);
    let raw = format!(
        "tree {}\nparent {}\ncommitter Test Committer <committer@example.test> 981173106 -0700\n\nmissing author\n",
        tree.trim(),
        parent.trim()
    );
    let mut child = Command::new("git")
        .args([
            "-C",
            path.to_str().unwrap(),
            "hash-object",
            "--literally",
            "-t",
            "commit",
            "-w",
            "--stdin",
        ])
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .unwrap();
    child
        .stdin
        .take()
        .unwrap()
        .write_all(raw.as_bytes())
        .unwrap();
    let output = child.wait_with_output().unwrap();
    assert!(
        output.status.success(),
        "git hash-object: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout).unwrap().trim().to_owned()
}

fn git(path: &Path, args: &[&str]) {
    let output = Command::new("git")
        .args(["-C", path.to_str().unwrap()])
        .args(args)
        .env("GIT_AUTHOR_DATE", "2026-01-02T03:04:05+0000")
        .env("GIT_COMMITTER_DATE", "2026-01-02T03:04:05+0000")
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "git {args:?}: {}",
        String::from_utf8_lossy(&output.stderr)
    );
}

fn git_output(path: &Path, args: &[&str]) -> String {
    let output = Command::new("git")
        .args(["-C", path.to_str().unwrap()])
        .args(args)
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "git {args:?}: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout).unwrap()
}

fn raw_path(_repo: &Path) -> PathBuf {
    PathBuf::from("raw-\n雪-name")
}

#[cfg(unix)]
fn path_bytes(path: &Path) -> &[u8] {
    use std::os::unix::ffi::OsStrExt;
    path.as_os_str().as_bytes()
}

#[cfg(not(unix))]
fn path_bytes(path: &Path) -> &[u8] {
    path.to_str().unwrap().as_bytes()
}

#[cfg(unix)]
fn set_executable(path: &Path) {
    use std::os::unix::fs::PermissionsExt;
    let mut permissions = fs::metadata(path).unwrap().permissions();
    permissions.set_mode(0o755);
    fs::set_permissions(path, permissions).unwrap();
}

#[cfg(not(unix))]
fn set_executable(_path: &Path) {}

fn write_frame(
    output: &mut impl Write,
    tag: [u8; 4],
    batch: u64,
    payload: &[u8],
) -> std::io::Result<()> {
    let length = u32::try_from(12 + payload.len()).unwrap();
    output.write_all(&length.to_be_bytes())?;
    output.write_all(&tag)?;
    output.write_all(&batch.to_be_bytes())?;
    output.write_all(payload)
}

fn read_frame(input: &mut impl Read) -> std::io::Result<Frame> {
    let mut prefix = [0; 4];
    input.read_exact(&mut prefix)?;
    let length = usize::try_from(u32::from_be_bytes(prefix)).unwrap();
    assert!((12..=16 << 20).contains(&length));
    let mut bytes = vec![0; length];
    input.read_exact(&mut bytes)?;
    Ok(Frame {
        tag: bytes[..4].try_into().unwrap(),
        batch: u64::from_be_bytes(bytes[4..12].try_into().unwrap()),
        payload: bytes[12..].to_vec(),
    })
}

fn append_bytes(output: &mut Vec<u8>, bytes: &[u8]) {
    output.extend_from_slice(&u32::try_from(bytes.len()).unwrap().to_be_bytes());
    output.extend_from_slice(bytes);
}

struct Decoder<'a> {
    bytes: &'a [u8],
    at: usize,
}

impl Decoder<'_> {
    fn bytes(&mut self) -> Vec<u8> {
        let length = usize::try_from(self.u32()).unwrap();
        let value = self.bytes[self.at..self.at + length].to_vec();
        self.at += length;
        value
    }

    fn u32(&mut self) -> u32 {
        let value = u32::from_be_bytes(self.bytes[self.at..self.at + 4].try_into().unwrap());
        self.at += 4;
        value
    }

    fn u64(&mut self) -> u64 {
        let value = u64::from_be_bytes(self.bytes[self.at..self.at + 8].try_into().unwrap());
        self.at += 8;
        value
    }

    fn byte(&mut self) -> u8 {
        let value = self.bytes[self.at];
        self.at += 1;
        value
    }
}

fn decode_file(payload: &[u8]) -> FileEvent {
    let mut decoder = Decoder {
        bytes: payload,
        at: 0,
    };
    let event = FileEvent {
        commit: decoder.bytes(),
        status: decoder.byte(),
        old_mode: decoder.u32(),
        new_mode: decoder.u32(),
        old_oid: decoder.bytes(),
        new_oid: decoder.bytes(),
        old_path: decoder.bytes(),
        new_path: decoder.bytes(),
        binary: decoder.byte() == 1,
    };
    assert_eq!(decoder.at, payload.len());
    event
}

fn decode_hunk(payload: &[u8]) -> HunkEvent {
    let mut decoder = Decoder {
        bytes: payload,
        at: 0,
    };
    let event = HunkEvent {
        commit: decoder.bytes(),
        path: decoder.bytes(),
        position: decoder.u64(),
        added: decoder.bytes(),
        missing: decoder.byte() == 1,
    };
    assert_eq!(decoder.at, payload.len());
    event
}

fn decode_hex(hex: &str) -> Vec<u8> {
    assert_eq!(hex.len() % 2, 0);
    hex.as_bytes()
        .chunks_exact(2)
        .map(|pair| {
            let digit = |byte| match byte {
                b'0'..=b'9' => byte - b'0',
                b'a'..=b'f' => byte - b'a' + 10,
                _ => panic!("invalid hex"),
            };
            digit(pair[0]) << 4 | digit(pair[1])
        })
        .collect()
}
