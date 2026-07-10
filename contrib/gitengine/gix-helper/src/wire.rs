use std::collections::HashSet;
use std::io::{self, Read, Write};

pub(crate) const VERSION: u16 = 1;
pub(crate) const MAX_FRAME: u32 = 16 << 20;
pub(crate) const MAX_RECORD: usize = 256 << 20;

pub(crate) const HELO: [u8; 4] = *b"HELO";
pub(crate) const BGIN: [u8; 4] = *b"BGIN";
pub(crate) const CMIT: [u8; 4] = *b"CMIT";
pub(crate) const FBEG: [u8; 4] = *b"FBEG";
pub(crate) const HUNK: [u8; 4] = *b"HUNK";
pub(crate) const HBGN: [u8; 4] = *b"HBGN";
pub(crate) const HADD: [u8; 4] = *b"HADD";
pub(crate) const HEND: [u8; 4] = *b"HEND";
pub(crate) const FEND: [u8; 4] = *b"FEND";
pub(crate) const CEND: [u8; 4] = *b"CEND";
pub(crate) const BEND: [u8; 4] = *b"BEND";
pub(crate) const ERRO: [u8; 4] = *b"ERRO";
pub(crate) const QUIT: [u8; 4] = *b"QUIT";

#[derive(Debug, Eq, PartialEq)]
pub(crate) struct Frame {
    pub tag: [u8; 4],
    pub batch_id: u64,
    pub payload: Vec<u8>,
}

pub(crate) fn read_frame(input: &mut impl Read) -> io::Result<Frame> {
    let mut prefix = [0; 4];
    input.read_exact(&mut prefix)?;
    let length = u32::from_be_bytes(prefix);
    if length < 12 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "frame shorter than header",
        ));
    }
    if length > MAX_FRAME {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "frame exceeds 16 MiB limit",
        ));
    }
    let mut header = [0; 12];
    input.read_exact(&mut header)?;
    let mut tag = [0; 4];
    tag.copy_from_slice(&header[..4]);
    let batch_id = u64::from_be_bytes(header[4..].try_into().expect("fixed-width batch ID"));
    let payload_len = usize::try_from(length - 12).expect("u32 fits usize");
    let mut payload = vec![0; payload_len];
    input.read_exact(&mut payload)?;
    Ok(Frame {
        tag,
        batch_id,
        payload,
    })
}

pub(crate) fn write_frame(
    output: &mut impl Write,
    tag: [u8; 4],
    batch_id: u64,
    payload: &[u8],
) -> io::Result<()> {
    let length = 12usize
        .checked_add(payload.len())
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "frame length overflow"))?;
    if length > usize::try_from(MAX_FRAME).expect("u32 fits usize") {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "frame exceeds 16 MiB limit",
        ));
    }
    output.write_all(
        &u32::try_from(length)
            .expect("bounded frame length")
            .to_be_bytes(),
    )?;
    output.write_all(&tag)?;
    output.write_all(&batch_id.to_be_bytes())?;
    output.write_all(payload)
}

pub(crate) fn hello_payload(oid_len: usize) -> Vec<u8> {
    let mut payload = Vec::with_capacity(6);
    payload.extend_from_slice(&VERSION.to_be_bytes());
    payload.extend_from_slice(
        &u16::try_from(oid_len)
            .expect("OID width fits u16")
            .to_be_bytes(),
    );
    payload.extend_from_slice(&[0, 0]);
    payload
}

pub(crate) fn decode_batch(frame: &Frame, oid_len: usize) -> io::Result<Vec<Vec<u8>>> {
    if frame.tag != BGIN || frame.batch_id == 0 {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "invalid BGIN frame",
        ));
    }
    let mut decoder = Decoder::new(&frame.payload);
    let count = usize::try_from(decoder.u32()?).expect("u32 fits usize");
    let minimum = count
        .checked_mul(4 + oid_len)
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidData, "commit count overflow"))?;
    if minimum > decoder.remaining() {
        return Err(io::Error::new(
            io::ErrorKind::InvalidData,
            "commit count exceeds payload",
        ));
    }
    let mut seen = HashSet::with_capacity(count);
    let mut commits = Vec::with_capacity(count);
    for _ in 0..count {
        let oid = decoder.bytes()?.to_vec();
        if oid.len() != oid_len {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "OID width differs from HELO",
            ));
        }
        if !seen.insert(oid.clone()) {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "duplicate OID in batch",
            ));
        }
        commits.push(oid);
    }
    decoder.finish()?;
    Ok(commits)
}

pub(crate) fn append_bytes(payload: &mut Vec<u8>, bytes: &[u8]) -> io::Result<()> {
    let length = u32::try_from(bytes.len()).map_err(|_| {
        io::Error::new(
            io::ErrorKind::InvalidInput,
            "byte string exceeds protocol limit",
        )
    })?;
    payload.extend_from_slice(&length.to_be_bytes());
    payload.extend_from_slice(bytes);
    Ok(())
}

pub(crate) fn write_hunk(
    output: &mut impl Write,
    batch_id: u64,
    commit: &[u8],
    path: &[u8],
    new_position: u64,
    added: &[u8],
    missing_final_newline: bool,
) -> io::Result<()> {
    if added.len() > MAX_RECORD {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "hunk exceeds 256 MiB limit",
        ));
    }
    let mut payload = Vec::with_capacity(
        4 + commit.len() + 4 + path.len() + 8 + 4 + added.len().min(64 << 10) + 1,
    );
    append_bytes(&mut payload, commit)?;
    append_bytes(&mut payload, path)?;
    payload.extend_from_slice(&new_position.to_be_bytes());
    let fixed_len = payload.len();
    let regular_len = 12usize
        .checked_add(fixed_len)
        .and_then(|n| n.checked_add(4 + added.len() + 1))
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "hunk length overflow"))?;
    if regular_len <= usize::try_from(MAX_FRAME).expect("u32 fits usize") {
        append_bytes(&mut payload, added)?;
        payload.push(u8::from(missing_final_newline));
        return write_frame(output, HUNK, batch_id, &payload);
    }
    if 12 + payload.len() > usize::try_from(MAX_FRAME).expect("u32 fits usize") {
        return Err(io::Error::new(
            io::ErrorKind::InvalidInput,
            "HBGN metadata exceeds frame limit",
        ));
    }
    write_frame(output, HBGN, batch_id, &payload)?;
    let chunk_size = usize::try_from(MAX_FRAME - 12).expect("u32 fits usize");
    for chunk in added.chunks(chunk_size) {
        write_frame(output, HADD, batch_id, chunk)?;
    }
    write_frame(output, HEND, batch_id, &[u8::from(missing_final_newline)])
}

pub(crate) fn error_payload(kind: u8, operation: &str, message: &str) -> Vec<u8> {
    let mut payload = vec![kind];
    // Error strings originate locally and are bounded well below a frame.
    let _ = append_bytes(&mut payload, operation.as_bytes());
    let _ = append_bytes(&mut payload, message.as_bytes());
    payload
}

pub(crate) struct Decoder<'a> {
    input: &'a [u8],
    position: usize,
}

impl<'a> Decoder<'a> {
    pub(crate) fn new(input: &'a [u8]) -> Self {
        Self { input, position: 0 }
    }

    fn remaining(&self) -> usize {
        self.input.len() - self.position
    }

    pub(crate) fn u32(&mut self) -> io::Result<u32> {
        if self.remaining() < 4 {
            return Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "truncated u32",
            ));
        }
        let value = u32::from_be_bytes(
            self.input[self.position..self.position + 4]
                .try_into()
                .expect("checked width"),
        );
        self.position += 4;
        Ok(value)
    }

    pub(crate) fn bytes(&mut self) -> io::Result<&'a [u8]> {
        let length = usize::try_from(self.u32()?).expect("u32 fits usize");
        if length > self.remaining() {
            return Err(io::Error::new(
                io::ErrorKind::UnexpectedEof,
                "truncated bytes",
            ));
        }
        let bytes = &self.input[self.position..self.position + length];
        self.position += length;
        Ok(bytes)
    }

    pub(crate) fn finish(self) -> io::Result<()> {
        if self.position == self.input.len() {
            Ok(())
        } else {
            Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "trailing payload bytes",
            ))
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn frame_bytes(tag: [u8; 4], batch: u64, payload: &[u8]) -> Vec<u8> {
        let mut out = Vec::new();
        write_frame(&mut out, tag, batch, payload).unwrap();
        out
    }

    #[test]
    fn frame_round_trip_preserves_arbitrary_bytes() {
        let payload = b"\0\xff\nraw";
        let frame = read_frame(&mut frame_bytes(CMIT, 9, payload).as_slice()).unwrap();
        assert_eq!(
            frame,
            Frame {
                tag: CMIT,
                batch_id: 9,
                payload: payload.to_vec()
            }
        );
    }

    #[test]
    fn rejects_short_truncated_and_oversized_frames() {
        assert!(read_frame(&mut [0, 0, 0, 11].as_slice()).is_err());
        assert!(read_frame(&mut [0, 0, 0, 12].as_slice()).is_err());
        let too_large = (MAX_FRAME + 1).to_be_bytes();
        assert!(read_frame(&mut too_large.as_slice()).is_err());
    }

    #[test]
    fn batch_supports_both_hash_widths_and_rejects_duplicates() {
        for width in [20, 32] {
            let oid = vec![7; width];
            let mut payload = 1u32.to_be_bytes().to_vec();
            append_bytes(&mut payload, &oid).unwrap();
            let got = decode_batch(
                &Frame {
                    tag: BGIN,
                    batch_id: 1,
                    payload,
                },
                width,
            )
            .unwrap();
            assert_eq!(got, vec![oid]);
        }
        let oid = vec![1; 20];
        let mut payload = 2u32.to_be_bytes().to_vec();
        append_bytes(&mut payload, &oid).unwrap();
        append_bytes(&mut payload, &oid).unwrap();
        assert!(
            decode_batch(
                &Frame {
                    tag: BGIN,
                    batch_id: 1,
                    payload
                },
                20
            )
            .is_err()
        );
    }

    #[test]
    fn streams_hunks_larger_than_sixteen_mib() {
        let added = vec![b'x'; (17 << 20) + 3];
        let mut out = Vec::new();
        write_hunk(&mut out, 4, &[1; 20], b"p", 8, &added, true).unwrap();
        let mut input = out.as_slice();
        let begin = read_frame(&mut input).unwrap();
        assert_eq!(begin.tag, HBGN);
        let mut reconstructed = Vec::new();
        loop {
            let frame = read_frame(&mut input).unwrap();
            match frame.tag {
                HADD => reconstructed.extend_from_slice(&frame.payload),
                HEND => {
                    assert_eq!(frame.payload, [1]);
                    break;
                }
                tag => panic!("unexpected tag {tag:?}"),
            }
        }
        assert_eq!(reconstructed, added);
        assert!(input.is_empty());
    }
}
