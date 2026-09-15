//! Capture-date candidates only; timezone policy belongs to the Agent.
use exif::{In, Tag, Value};
use serde_json::{json, Value as Json};
use std::fs::File;
use std::io::{self, BufReader, Read, Seek, SeekFrom, Write};
use std::path::Path;

const MAX_METADATA_BYTES: usize = 16 * 1024 * 1024;
const PNG_SIGNATURE: &[u8; 8] = b"\x89PNG\r\n\x1a\n";

pub fn inspect_image(input: &Path, stdout: &mut dyn Write) -> Result<(), String> {
    let file = File::open(input).map_err(|_| "open image metadata failed".to_string())?;
    let mut reader = MetadataReader {
        file,
        remaining: MAX_METADATA_BYTES,
    };
    let candidates = match read_image_exif(&mut reader) {
        Ok(exif) => image_candidates(&exif),
        // Missing/invalid EXIF must not prevent an otherwise usable image from
        // entering the gallery. The Agent records its explicit fallback source.
        Err(exif::Error::Io(_)) => return Err("read image metadata failed".into()),
        Err(_) => Vec::new(),
    };
    writeln!(
        stdout,
        "{}",
        json!({
            "schemaVersion": 1, "ok": true, "operation": "inspect-image",
            "media": {"mediaType": "image", "captureDates": candidates}
        })
    )
    .map_err(|_| "write image metadata failed".to_string())
}

// The EXIF library's PNG and WebP readers discard payloads with Read, spending
// the metadata budget on pixels. Walk those containers with Seek instead, then
// delegate only bounded EXIF bytes to the library's TIFF/EXIF parser.
fn read_image_exif<R: Read + Seek>(reader: &mut R) -> Result<exif::Exif, exif::Error> {
    let mut signature = [0; 8];
    read_container_bytes(reader, &mut signature)?;
    if &signature == PNG_SIGNATURE {
        return read_png_exif(reader);
    }
    if &signature[..4] == b"RIFF" {
        return read_webp_exif(
            reader,
            u32::from_le_bytes(signature[4..].try_into().unwrap()),
        );
    }
    reader.seek(SeekFrom::Start(0))?;
    exif::Reader::new().read_from_container(&mut BufReader::new(reader))
}

fn read_container_bytes(reader: &mut impl Read, bytes: &mut [u8]) -> Result<(), exif::Error> {
    reader.read_exact(bytes).map_err(|error| {
        if error.kind() == io::ErrorKind::UnexpectedEof {
            exif::Error::InvalidFormat("Truncated image metadata container")
        } else {
            exif::Error::Io(error)
        }
    })
}

// The PNG signature has already been consumed. Validate skipped extents against
// EOF, since seeking alone can succeed past the end of a truncated file.
fn read_png_exif<R: Read + Seek>(reader: &mut R) -> Result<exif::Exif, exif::Error> {
    let file_end = reader.seek(SeekFrom::End(0))?;
    reader.seek(SeekFrom::Start(8))?;
    loop {
        let mut header = [0; 8];
        read_container_bytes(reader, &mut header)?;
        let length = u32::from_be_bytes(header[..4].try_into().unwrap());
        if length > 0x7fff_ffff {
            return Err(exif::Error::InvalidFormat("Invalid PNG chunk length"));
        }
        let end = reader
            .stream_position()?
            .checked_add(u64::from(length) + 4)
            .filter(|end| *end <= file_end)
            .ok_or(exif::Error::InvalidFormat("Truncated PNG chunk"))?;
        match &header[4..] {
            b"eXIf" => return read_exif_payload(reader, length),
            b"IEND" => return Err(exif::Error::NotFound("PNG")),
            _ => {
                reader.seek(SeekFrom::Start(end))?;
            }
        }
    }
}

// RIFF's length excludes its first eight bytes but includes the WEBP tag.
// Every chunk, including its odd-length padding, must fit both RIFF and EOF.
// VP8/VP8L pixels, alpha planes, and ANMF frame bodies can all be skipped.
fn read_webp_exif<R: Read + Seek>(
    reader: &mut R,
    riff_length: u32,
) -> Result<exif::Exif, exif::Error> {
    let mut kind = [0; 4];
    read_container_bytes(reader, &mut kind)?;
    if &kind != b"WEBP" || riff_length < 4 {
        return Err(exif::Error::InvalidFormat("Invalid WebP RIFF header"));
    }
    let riff_end = u64::from(riff_length) + 8;
    if riff_end > reader.seek(SeekFrom::End(0))? {
        return Err(exif::Error::InvalidFormat("Truncated WebP RIFF container"));
    }
    let mut position = reader.seek(SeekFrom::Start(12))?;
    while position < riff_end {
        if riff_end - position < 8 {
            return Err(exif::Error::InvalidFormat("Truncated WebP chunk header"));
        }
        let mut header = [0; 8];
        read_container_bytes(reader, &mut header)?;
        let length = u32::from_le_bytes(header[4..].try_into().unwrap());
        let end = position + 8 + u64::from(length) + u64::from(length % 2);
        if end > riff_end {
            return Err(exif::Error::InvalidFormat(
                "WebP chunk exceeds RIFF boundary",
            ));
        }
        if &header[..4] == b"EXIF" {
            return read_exif_payload(reader, length);
        }
        position = reader.seek(SeekFrom::Start(end))?;
    }
    Err(exif::Error::NotFound("WebP"))
}

fn read_exif_payload(reader: &mut impl Read, length: u32) -> Result<exif::Exif, exif::Error> {
    if length as usize > MAX_METADATA_BYTES {
        return Err(exif::Error::Io(io::Error::new(
            io::ErrorKind::InvalidData,
            "image metadata read limit exceeded",
        )));
    }
    let mut data = vec![0; length as usize];
    read_container_bytes(reader, &mut data)?;
    exif::Reader::new().read_raw(data)
}

// Seek past media payloads, but bound bytes actually read even if a malformed
// container declares a huge metadata block. No pixel decoding is needed.
struct MetadataReader<R> {
    file: R,
    remaining: usize,
}

impl<R: Read> Read for MetadataReader<R> {
    fn read(&mut self, buffer: &mut [u8]) -> io::Result<usize> {
        if buffer.is_empty() {
            return Ok(0);
        }
        if self.remaining == 0 {
            return Err(io::Error::new(
                io::ErrorKind::InvalidData,
                "image metadata read limit exceeded",
            ));
        }
        let limit = buffer.len().min(self.remaining);
        let read = self.file.read(&mut buffer[..limit])?;
        self.remaining -= read;
        Ok(read)
    }
}

impl<R: Seek> Seek for MetadataReader<R> {
    fn seek(&mut self, position: SeekFrom) -> io::Result<u64> {
        self.file.seek(position)
    }
}

fn ascii(exif: &exif::Exif, tag: Tag) -> Option<String> {
    let Value::Ascii(values) = &exif.get_field(tag, In::PRIMARY)?.value else {
        return None;
    };
    let text = std::str::from_utf8(values.first()?)
        .ok()?
        .trim_matches('\0')
        .trim();
    (!text.is_empty()).then(|| text.to_owned())
}

fn image_candidates(exif: &exif::Exif) -> Vec<Json> {
    [
        (
            Tag::DateTimeOriginal,
            Tag::OffsetTimeOriginal,
            Tag::SubSecTimeOriginal,
            "exif_original",
        ),
        (
            Tag::DateTimeDigitized,
            Tag::OffsetTimeDigitized,
            Tag::SubSecTimeDigitized,
            "exif_digitized",
        ),
    ]
    .into_iter()
    .filter_map(|(date, offset, subsec, source)| {
        Some(
            json!({"value": ascii(exif, date)?, "offset": ascii(exif, offset),
            "subsecond": ascii(exif, subsec), "source": source}),
        )
    })
    .collect()
}

pub fn video_candidates(probe: &Json) -> Vec<Json> {
    let mut dates = Vec::new();
    let mut push = |tags: &Json, key: &str, source: &str| {
        if let Some(value) = tags[key].as_str().filter(|value| !value.trim().is_empty()) {
            dates.push(json!({"value": value, "source": source}));
        }
    };
    push(
        &probe["format"]["tags"],
        "com.apple.quicktime.creationdate",
        "quicktime_creationdate",
    );
    push(
        &probe["format"]["tags"],
        "creation_time",
        "container_creation_time",
    );
    if let Some(streams) = probe["streams"].as_array() {
        for stream in streams {
            if stream["codec_type"] == "video" {
                push(
                    &stream["tags"],
                    "creation_time",
                    "video_stream_creation_time",
                );
            }
        }
    }
    dates
}

pub fn video_duration_ms(probe: &Json) -> Option<u64> {
    let mut values = vec![&probe["format"]["duration"]];
    if let Some(streams) = probe["streams"].as_array() {
        values.extend(streams.iter().map(|stream| &stream["duration"]));
    }
    let seconds = values
        .into_iter()
        .filter_map(|value| value.as_str()?.parse::<f64>().ok())
        .filter(|value| value.is_finite() && *value > 0.0)
        .fold(0.0_f64, f64::max);
    let milliseconds = (seconds * 1000.0).round();
    (milliseconds.is_finite() && milliseconds > 0.0 && milliseconds < u64::MAX as f64)
        .then_some(milliseconds as u64)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn metadata_read_budget_is_not_reset_by_seek() {
        let mut reader = MetadataReader {
            file: File::open(concat!(env!("CARGO_MANIFEST_DIR"), "/Cargo.toml")).unwrap(),
            remaining: 8,
        };
        let mut bytes = [0; 8];
        reader.read_exact(&mut bytes).unwrap();
        reader.seek(SeekFrom::Start(0)).unwrap();
        assert_eq!(
            reader.read(&mut bytes).unwrap_err().kind(),
            io::ErrorKind::InvalidData
        );
    }

    #[test]
    fn image_dates_round_trip_through_jpeg_png_webp_and_heif() {
        use exif::experimental::Writer;
        use std::io::Cursor;
        let fields = [
            (Tag::DateTimeOriginal, "2026:09:06 12:34:56"),
            (Tag::OffsetTimeOriginal, "+09:00"),
            (Tag::SubSecTimeOriginal, "123"),
            (Tag::DateTimeDigitized, "2026:09:07 01:02:03"),
            // Modification time is deliberately excluded from capture dates.
            (Tag::DateTime, "2026:09:13 22:19:29"),
        ]
        .map(|(tag, value)| exif::Field {
            tag,
            ifd_num: In::PRIMARY,
            value: Value::Ascii(vec![value.as_bytes().to_vec()]),
        });
        let mut writer = Writer::new();
        for field in &fields {
            writer.push_field(field);
        }
        let mut tiff = Cursor::new(Vec::new());
        writer.write(&mut tiff, true).unwrap();
        let tiff = tiff.into_inner();

        let mut jpeg = vec![0xff, 0xd8, 0xff, 0xe1];
        jpeg.extend_from_slice(&((tiff.len() + 8) as u16).to_be_bytes());
        jpeg.extend_from_slice(b"Exif\0\0");
        jpeg.extend_from_slice(&tiff);
        jpeg.extend_from_slice(&[0xff, 0xd9]);
        let mut png = b"\x89PNG\r\n\x1a\n".to_vec();
        png.extend_from_slice(&(tiff.len() as u32).to_be_bytes());
        png.extend_from_slice(b"eXIf");
        png.extend_from_slice(&tiff);
        png.extend_from_slice(&[0; 4]); // The metadata parser does not decode PNG pixels.
        let mut webp = b"RIFF".to_vec();
        webp.extend_from_slice(&((12 + tiff.len() + tiff.len() % 2) as u32).to_le_bytes());
        webp.extend_from_slice(b"WEBPEXIF");
        webp.extend_from_slice(&(tiff.len() as u32).to_le_bytes());
        webp.extend_from_slice(&tiff);
        if tiff.len() % 2 != 0 {
            webp.push(0);
        }

        // A metadata-only HEIF container, with one Exif item inside idat.
        fn boxed(kind: &[u8; 4], body: &[u8]) -> Vec<u8> {
            let mut out = ((body.len() + 8) as u32).to_be_bytes().to_vec();
            out.extend_from_slice(kind);
            out.extend_from_slice(body);
            out
        }
        let mut heif = boxed(b"ftyp", b"mif1\0\0\0\0mif1");
        let mut item = vec![0, 0, 0, 0];
        item.extend_from_slice(&tiff);
        let mut iloc = vec![1, 0, 0, 0, 0x44, 0, 0, 1, 0, 1, 0, 1, 0, 0, 0, 1];
        iloc.extend_from_slice(&0u32.to_be_bytes());
        iloc.extend_from_slice(&(item.len() as u32).to_be_bytes());
        let infe = boxed(b"infe", b"\x02\0\0\0\0\x01\0\0Exif\0");
        let mut iinf = vec![0, 0, 0, 0, 0, 1];
        iinf.extend_from_slice(&infe);
        let mut meta = vec![0, 0, 0, 0];
        meta.extend_from_slice(&boxed(b"iinf", &iinf));
        meta.extend_from_slice(&boxed(b"iloc", &iloc));
        meta.extend_from_slice(&boxed(b"idat", &item));
        heif.extend_from_slice(&boxed(b"meta", &meta));
        for (kind, bytes) in [("jpeg", jpeg), ("png", png), ("webp", webp), ("heif", heif)] {
            let parsed = read_image_exif(&mut Cursor::new(bytes)).expect(kind);
            let candidates = image_candidates(&parsed);
            assert_eq!(candidates.len(), 2, "{kind}");
            assert_eq!(candidates[0]["value"], "2026:09:06 12:34:56", "{kind}");
            assert_eq!(candidates[0]["offset"], "+09:00", "{kind}");
            assert_eq!(candidates[0]["subsecond"], "123", "{kind}");
            assert_eq!(candidates[1]["source"], "exif_digitized", "{kind}");
        }
    }

    fn png_chunk(png: &mut Vec<u8>, kind: &[u8; 4], data: &[u8]) {
        png.extend_from_slice(&(data.len() as u32).to_be_bytes());
        png.extend_from_slice(kind);
        png.extend_from_slice(data);
        png.extend_from_slice(&[0; 4]); // Inspection does not validate pixel CRCs.
    }

    fn metadata_reader(bytes: Vec<u8>) -> MetadataReader<io::Cursor<Vec<u8>>> {
        MetadataReader {
            file: io::Cursor::new(bytes),
            remaining: MAX_METADATA_BYTES,
        }
    }

    #[test]
    fn png_skips_large_payloads_without_dates() {
        let mut png = PNG_SIGNATURE.to_vec();
        let payload = vec![0; 9 * 1024 * 1024];
        png_chunk(&mut png, b"IDAT", &payload);
        png_chunk(&mut png, b"IDAT", &payload);
        png_chunk(&mut png, b"IEND", &[]);
        let mut reader = metadata_reader(png);
        assert!(matches!(
            read_image_exif(&mut reader),
            Err(exif::Error::NotFound(_))
        ));
        // Only the signature and three chunk headers should have been read.
        assert_eq!(MAX_METADATA_BYTES - reader.remaining, 32);
    }

    #[test]
    fn png_finds_exif_after_large_payload() {
        let field = exif::Field {
            tag: Tag::DateTimeOriginal,
            ifd_num: In::PRIMARY,
            value: Value::Ascii(vec![b"2026:09:06 12:34:56".to_vec()]),
        };
        let mut writer = exif::experimental::Writer::new();
        writer.push_field(&field);
        let mut tiff = io::Cursor::new(Vec::new());
        writer.write(&mut tiff, true).unwrap();
        let mut png = PNG_SIGNATURE.to_vec();
        png_chunk(&mut png, b"IDAT", &vec![0; 18 * 1024 * 1024]);
        png_chunk(&mut png, b"eXIf", tiff.get_ref());
        png_chunk(&mut png, b"IEND", &[]);
        let mut reader = metadata_reader(png);
        let exif = read_image_exif(&mut reader).unwrap();
        assert_eq!(image_candidates(&exif)[0]["value"], "2026:09:06 12:34:56");
        assert_eq!(
            MAX_METADATA_BYTES - reader.remaining,
            24 + tiff.get_ref().len()
        );
    }

    #[test]
    fn png_still_limits_exif_and_chunk_headers() {
        let mut png = PNG_SIGNATURE.to_vec();
        png_chunk(&mut png, b"eXIf", &vec![0; MAX_METADATA_BYTES + 1]);
        assert!(matches!(
            read_image_exif(&mut metadata_reader(png)),
            Err(exif::Error::Io(_))
        ));
        let mut png = PNG_SIGNATURE.to_vec();
        for _ in 0..10 {
            png_chunk(&mut png, b"IDAT", &[]);
        }
        let mut reader = metadata_reader(png);
        reader.remaining = 32;
        assert!(matches!(
            read_image_exif(&mut reader),
            Err(exif::Error::Io(_))
        ));
    }

    #[test]
    fn png_rejects_truncated_extents_and_invalid_lengths() {
        for length in [32u32, 0x8000_0000, u32::MAX] {
            let mut png = PNG_SIGNATURE.to_vec();
            png.extend_from_slice(&length.to_be_bytes());
            png.extend_from_slice(b"IDAT");
            assert!(matches!(
                read_image_exif(&mut metadata_reader(png)),
                Err(exif::Error::InvalidFormat(_))
            ));
        }
        let mut png = PNG_SIGNATURE.to_vec();
        png_chunk(&mut png, b"eXIf", &[0; 8]);
        png.pop(); // Missing CRC byte must not count as a complete chunk.
        assert!(matches!(
            read_image_exif(&mut metadata_reader(png)),
            Err(exif::Error::InvalidFormat(_))
        ));
    }

    #[test]
    fn png_stops_at_iend() {
        let mut png = PNG_SIGNATURE.to_vec();
        png_chunk(&mut png, b"IEND", &[]);
        png_chunk(&mut png, b"eXIf", &vec![0; MAX_METADATA_BYTES + 1]);
        assert!(matches!(
            read_image_exif(&mut metadata_reader(png)),
            Err(exif::Error::NotFound(_))
        ));
    }

    fn webp_chunk(webp: &mut Vec<u8>, kind: &[u8; 4], data: &[u8]) {
        webp.extend_from_slice(kind);
        webp.extend_from_slice(&(data.len() as u32).to_le_bytes());
        webp.extend_from_slice(data);
        if data.len() % 2 != 0 {
            webp.push(0);
        }
        let length = (webp.len() - 8) as u32;
        webp[4..8].copy_from_slice(&length.to_le_bytes());
    }

    fn webp_header() -> Vec<u8> {
        b"RIFF\x04\0\0\0WEBP".to_vec()
    }

    fn date_exif() -> Vec<u8> {
        let field = exif::Field {
            tag: Tag::DateTimeOriginal,
            ifd_num: In::PRIMARY,
            value: Value::Ascii(vec![b"2026:09:06 12:34:56".to_vec()]),
        };
        let mut writer = exif::experimental::Writer::new();
        writer.push_field(&field);
        let mut tiff = io::Cursor::new(Vec::new());
        writer.write(&mut tiff, true).unwrap();
        tiff.into_inner()
    }

    #[test]
    fn webp_skips_large_pixel_and_animation_chunks_with_or_without_exif() {
        let payload = vec![0; 18 * 1024 * 1024 + 1]; // Exercise odd-length padding.
        for kind in [b"VP8 ", b"VP8L", b"ANMF"] {
            for with_exif in [false, true] {
                let mut webp = webp_header();
                webp_chunk(&mut webp, b"VP8X", &[0; 10]);
                webp_chunk(&mut webp, kind, &payload);
                let tiff = date_exif();
                if with_exif {
                    webp_chunk(&mut webp, b"EXIF", &tiff);
                }
                let mut reader = metadata_reader(webp);
                let result = read_image_exif(&mut reader);
                if with_exif {
                    assert_eq!(
                        image_candidates(&result.unwrap())[0]["value"],
                        "2026:09:06 12:34:56"
                    );
                } else {
                    assert!(matches!(result, Err(exif::Error::NotFound(_))));
                }
                // VP8X and all pixel/animation bytes were skipped, not read.
                assert_eq!(
                    MAX_METADATA_BYTES - reader.remaining,
                    28 + if with_exif { 8 + tiff.len() } else { 0 }
                );
            }
        }
    }

    #[test]
    fn webp_limits_exif_and_structure_reads() {
        let mut webp = webp_header();
        webp_chunk(&mut webp, b"EXIF", &vec![0; MAX_METADATA_BYTES + 1]);
        assert!(matches!(
            read_image_exif(&mut metadata_reader(webp)),
            Err(exif::Error::Io(_))
        ));
        let mut webp = webp_header();
        for _ in 0..10 {
            webp_chunk(&mut webp, b"JUNK", &[]);
        }
        let mut reader = metadata_reader(webp);
        reader.remaining = 32;
        assert!(matches!(
            read_image_exif(&mut reader),
            Err(exif::Error::Io(_))
        ));
    }

    #[test]
    fn webp_checks_riff_chunk_and_padding_boundaries() {
        let mut valid = webp_header();
        webp_chunk(&mut valid, b"VP8L", &[0; 3]);
        for length in [0u32, 3, 5, 11, 15, 17, u32::MAX] {
            let mut webp = valid.clone();
            webp[4..8].copy_from_slice(&length.to_le_bytes());
            assert!(
                matches!(
                    read_image_exif(&mut metadata_reader(webp)),
                    Err(exif::Error::InvalidFormat(_))
                ),
                "RIFF length {length}"
            );
        }
        for length in [5u32, u32::MAX] {
            let mut webp = valid.clone();
            webp[16..20].copy_from_slice(&length.to_le_bytes());
            assert!(matches!(
                read_image_exif(&mut metadata_reader(webp)),
                Err(exif::Error::InvalidFormat(_))
            ));
        }
        valid.pop(); // RIFF fits EOF, but the odd-sized chunk lacks padding.
        let length = (valid.len() - 8) as u32;
        valid[4..8].copy_from_slice(&length.to_le_bytes());
        assert!(matches!(
            read_image_exif(&mut metadata_reader(valid)),
            Err(exif::Error::InvalidFormat(_))
        ));
        let mut truncated_exif = webp_header();
        webp_chunk(&mut truncated_exif, b"EXIF", &date_exif());
        truncated_exif.pop();
        assert!(matches!(
            read_image_exif(&mut metadata_reader(truncated_exif)),
            Err(exif::Error::InvalidFormat(_))
        ));
    }

    #[test]
    fn webp_ignores_exif_outside_riff_and_accepts_odd_exif_padding() {
        let mut webp = webp_header();
        webp_chunk(&mut webp, b"VP8L", &[0; 3]);
        let declared_length = webp[4..8].to_vec();
        let mut tiff = date_exif();
        if tiff.len() % 2 == 0 {
            tiff.push(0);
        }
        webp_chunk(&mut webp, b"EXIF", &tiff);
        assert!(read_image_exif(&mut metadata_reader(webp.clone())).is_ok());
        webp[4..8].copy_from_slice(&declared_length);
        assert!(matches!(
            read_image_exif(&mut metadata_reader(webp)),
            Err(exif::Error::NotFound(_))
        ));
    }

    #[test]
    fn video_prefers_camera_date_then_container_then_video_stream() {
        let probe = json!({"format": {"duration": "12.3", "tags": {
            "creation_time": "2026-09-13T13:19:29Z",
            "com.apple.quicktime.creationdate": "2026-09-06T12:00:00+0900"
        }}, "streams": [
            {"codec_type": "audio", "duration": "13", "tags": {"creation_time": "wrong"}},
            {"codec_type": "video", "duration": "N/A", "tags": {"creation_time": "2026-09-06T03:00:00Z"}}
        ]});
        let dates = video_candidates(&probe);
        assert_eq!(dates.len(), 3);
        assert_eq!(dates[0]["source"], "quicktime_creationdate");
        assert_eq!(dates[2]["source"], "video_stream_creation_time");
        assert_eq!(video_duration_ms(&probe), Some(13_000));
        assert_eq!(
            video_duration_ms(&json!({"format": {"duration": "NaN"}})),
            None
        );
    }
}
