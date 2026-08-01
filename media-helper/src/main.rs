use std::env;
use std::ffi::OsString;
use std::fs;
use std::io::{self, Write};
use std::path::{Path, PathBuf};
use std::process::Command;

const SCHEMA_VERSION: u32 = 1;

#[derive(Clone, Debug, Eq, PartialEq)]
struct BackendStatus {
    name: &'static str,
    status: &'static str,
    path: Option<PathBuf>,
    source: &'static str,
    warnings: Vec<String>,
}

fn main() {
    let code = run(env::args_os().skip(1).collect(), &mut io::stdout(), &mut io::stderr());
    if code != 0 {
        std::process::exit(code);
    }
}

fn run(args: Vec<OsString>, stdout: &mut dyn Write, stderr: &mut dyn Write) -> i32 {
    if args.is_empty() {
        let _ = usage(stderr);
        return 2;
    }
    let command = args[0].to_string_lossy();
    match command.as_ref() {
        "health" => health(&args[1..], stdout, stderr),
        "render-image" => render_image(&args[1..], stdout, stderr),
        "render-video-poster" => render_video_poster(&args[1..], stdout, stderr),
        "version" => {
            let _ = writeln!(stdout, "{}", env!("CARGO_PKG_VERSION"));
            0
        }
        "help" | "--help" | "-h" => {
            let _ = usage(stdout);
            0
        }
        _ => {
            let _ = writeln!(stderr, "unknown command: {command}");
            let _ = usage(stderr);
            2
        }
    }
}

fn usage(writer: &mut dyn Write) -> io::Result<()> {
    writeln!(
        writer,
        "Usage:\n  timich-media-helper health --json\n  timich-media-helper render-image --input <image> --output <rendition.jpg> --max-edge <pixels> --quality <1-100>\n  timich-media-helper render-video-poster --input <video> --output <poster.jpg>\n  timich-media-helper version"
    )
}

fn health(args: &[OsString], stdout: &mut dyn Write, stderr: &mut dyn Write) -> i32 {
    let mut json = false;
    for arg in args {
        match arg.to_string_lossy().as_ref() {
            "--json" => json = true,
            "--help" | "-h" => {
                let _ = usage(stdout);
                return 0;
            }
            other => {
                let _ = writeln!(stderr, "unexpected health argument: {other}");
                return 2;
            }
        }
    }
    if !json {
        let _ = writeln!(stderr, "health currently requires --json");
        return 2;
    }

    let vips = detect_backend(
        "libvips-cli",
        media_binary_name("vips"),
        &["TIMICH_MEDIA_HELPER_VIPS_PATH", "TIMICH_AGENT_VIPS_PATH"],
        &["media-runtime", "libvips", "bin"],
    );
    let ffmpeg = detect_backend(
        "ffmpeg-cli",
        media_binary_name("ffmpeg"),
        &["TIMICH_MEDIA_HELPER_FFMPEG_PATH", "TIMICH_AGENT_FFMPEG_PATH"],
        &["media-runtime", "ffmpeg", "bin"],
    );
    let ffprobe = detect_backend(
        "ffprobe-cli",
        media_binary_name("ffprobe"),
        &["TIMICH_MEDIA_HELPER_FFPROBE_PATH"],
        &["media-runtime", "ffmpeg", "bin"],
    );

    let response = health_json(&vips, &ffmpeg, &ffprobe);
    let _ = writeln!(stdout, "{response}");
    0
}

fn render_image(args: &[OsString], stdout: &mut dyn Write, stderr: &mut dyn Write) -> i32 {
    let mut input: Option<PathBuf> = None;
    let mut output: Option<PathBuf> = None;
    let mut max_edge: Option<u32> = None;
    let mut quality: Option<u8> = None;
    let mut index = 0;
    while index < args.len() {
        match args[index].to_string_lossy().as_ref() {
            "--input" => {
                index += 1;
                if index >= args.len() {
                    let _ = writeln!(stderr, "--input requires a path");
                    return 2;
                }
                input = Some(PathBuf::from(args[index].clone()));
            }
            "--output" => {
                index += 1;
                if index >= args.len() {
                    let _ = writeln!(stderr, "--output requires a path");
                    return 2;
                }
                output = Some(PathBuf::from(args[index].clone()));
            }
            "--max-edge" => {
                index += 1;
                if index >= args.len() {
                    let _ = writeln!(stderr, "--max-edge requires a positive integer");
                    return 2;
                }
                let value = args[index].to_string_lossy();
                match value.parse::<u32>() {
                    Ok(parsed) if parsed > 0 => max_edge = Some(parsed),
                    _ => {
                        let _ = writeln!(stderr, "--max-edge must be a positive integer");
                        return 2;
                    }
                }
            }
            "--quality" => {
                index += 1;
                if index >= args.len() {
                    let _ = writeln!(stderr, "--quality requires an integer from 1 to 100");
                    return 2;
                }
                let value = args[index].to_string_lossy();
                match value.parse::<u8>() {
                    Ok(parsed) if (1..=100).contains(&parsed) => quality = Some(parsed),
                    _ => {
                        let _ = writeln!(stderr, "--quality must be an integer from 1 to 100");
                        return 2;
                    }
                }
            }
            "--help" | "-h" => {
                let _ = usage(stdout);
                return 0;
            }
            other => {
                let _ = writeln!(stderr, "unexpected render-image argument: {other}");
                return 2;
            }
        }
        index += 1;
    }

    let Some(input) = input else {
        let _ = writeln!(stderr, "render-image requires --input");
        return 2;
    };
    let Some(output) = output else {
        let _ = writeln!(stderr, "render-image requires --output");
        return 2;
    };
    let Some(max_edge) = max_edge else {
        let _ = writeln!(stderr, "render-image requires --max-edge");
        return 2;
    };
    let Some(quality) = quality else {
        let _ = writeln!(stderr, "render-image requires --quality");
        return 2;
    };
    if !input.is_file() {
        let _ = writeln!(stderr, "input image is not a readable file: {}", input.display());
        return 1;
    }
    if let Some(parent) = output.parent() {
        if let Err(err) = fs::create_dir_all(parent) {
            let _ = writeln!(stderr, "create output directory: {err}");
            return 1;
        }
    }

    let vips = detect_backend(
        "libvips-cli",
        media_binary_name("vips"),
        &["TIMICH_MEDIA_HELPER_VIPS_PATH", "TIMICH_AGENT_VIPS_PATH"],
        &["media-runtime", "libvips", "bin"],
    );
    let Some(vips_path) = vips.path.as_deref() else {
        let _ = writeln!(stderr, "libvips runtime is not available");
        return 1;
    };
    if !vips.is_available() {
        let _ = writeln!(
            stderr,
            "libvips runtime is not usable: {}",
            json_string_array(&vips.warnings)
        );
        return 1;
    }

    let output_with_options = format!("{}[Q={quality},strip]", output.to_string_lossy());
    let max_edge_text = max_edge.to_string();
    let vips_output = Command::new(vips_path)
        .arg("thumbnail")
        .arg(&input)
        .arg(&output_with_options)
        .arg(&max_edge_text)
        .arg("--height")
        .arg(&max_edge_text)
        .arg("--size")
        .arg("down")
        .output();
    match vips_output {
        Ok(result) if result.status.success() => {}
        Ok(result) => {
            let message = String::from_utf8_lossy(&result.stderr);
            let fallback = String::from_utf8_lossy(&result.stdout);
            let message = if message.trim().is_empty() {
                fallback.trim()
            } else {
                message.trim()
            };
            let _ = writeln!(stderr, "libvips image render failed: {message}");
            return 1;
        }
        Err(err) => {
            let _ = writeln!(stderr, "start libvips image render: {err}");
            return 1;
        }
    }

    match fs::metadata(&output) {
        Ok(metadata) if metadata.len() > 0 => {}
        Ok(_) => {
            let _ = writeln!(stderr, "libvips produced an empty image rendition");
            return 1;
        }
        Err(err) => {
            let _ = writeln!(stderr, "libvips did not create image rendition output: {err}");
            return 1;
        }
    }

    let response = format!(
        concat!(
            "{{",
            "\"schemaVersion\":{schema_version},",
            "\"ok\":true,",
            "\"operation\":\"render-image\",",
            "\"backend\":\"libvips-cli\",",
            "\"outputPath\":{output_path}",
            "}}"
        ),
        schema_version = SCHEMA_VERSION,
        output_path = json_string(&output.to_string_lossy()),
    );
    let _ = writeln!(stdout, "{response}");
    0
}

fn render_video_poster(args: &[OsString], stdout: &mut dyn Write, stderr: &mut dyn Write) -> i32 {
    let mut input: Option<PathBuf> = None;
    let mut output: Option<PathBuf> = None;
    let mut index = 0;
    while index < args.len() {
        match args[index].to_string_lossy().as_ref() {
            "--input" => {
                index += 1;
                if index >= args.len() {
                    let _ = writeln!(stderr, "--input requires a path");
                    return 2;
                }
                input = Some(PathBuf::from(args[index].clone()));
            }
            "--output" => {
                index += 1;
                if index >= args.len() {
                    let _ = writeln!(stderr, "--output requires a path");
                    return 2;
                }
                output = Some(PathBuf::from(args[index].clone()));
            }
            "--help" | "-h" => {
                let _ = usage(stdout);
                return 0;
            }
            other => {
                let _ = writeln!(stderr, "unexpected render-video-poster argument: {other}");
                return 2;
            }
        }
        index += 1;
    }

    let Some(input) = input else {
        let _ = writeln!(stderr, "render-video-poster requires --input");
        return 2;
    };
    let Some(output) = output else {
        let _ = writeln!(stderr, "render-video-poster requires --output");
        return 2;
    };
    if !input.is_file() {
        let _ = writeln!(stderr, "input video is not a readable file: {}", input.display());
        return 1;
    }
    if let Some(parent) = output.parent() {
        if let Err(err) = fs::create_dir_all(parent) {
            let _ = writeln!(stderr, "create output directory: {err}");
            return 1;
        }
    }

    let ffmpeg = detect_backend(
        "ffmpeg-cli",
        media_binary_name("ffmpeg"),
        &["TIMICH_MEDIA_HELPER_FFMPEG_PATH", "TIMICH_AGENT_FFMPEG_PATH"],
        &["media-runtime", "ffmpeg", "bin"],
    );
    let Some(ffmpeg_path) = ffmpeg.path.as_deref() else {
        let _ = writeln!(stderr, "ffmpeg runtime is not available");
        return 1;
    };
    if !ffmpeg.is_available() {
        let _ = writeln!(
            stderr,
            "ffmpeg runtime is not usable: {}",
            json_string_array(&ffmpeg.warnings)
        );
        return 1;
    }

    let ffmpeg_output = Command::new(ffmpeg_path)
        .arg("-hide_banner")
        .arg("-loglevel")
        .arg("error")
        .arg("-nostdin")
        .arg("-y")
        .arg("-i")
        .arg(&input)
        .arg("-map")
        .arg("0:v:0")
        .arg("-frames:v")
        .arg("1")
        .arg("-an")
        .arg("-sn")
        .arg("-dn")
        .arg(&output)
        .output();
    match ffmpeg_output {
        Ok(result) if result.status.success() => {}
        Ok(result) => {
            let message = String::from_utf8_lossy(&result.stderr);
            let fallback = String::from_utf8_lossy(&result.stdout);
            let message = if message.trim().is_empty() {
                fallback.trim()
            } else {
                message.trim()
            };
            let _ = writeln!(stderr, "ffmpeg poster extraction failed: {message}");
            return 1;
        }
        Err(err) => {
            let _ = writeln!(stderr, "start ffmpeg poster extraction: {err}");
            return 1;
        }
    }

    match fs::metadata(&output) {
        Ok(metadata) if metadata.len() > 0 => {}
        Ok(_) => {
            let _ = writeln!(stderr, "ffmpeg produced an empty poster");
            return 1;
        }
        Err(err) => {
            let _ = writeln!(stderr, "ffmpeg did not create poster output: {err}");
            return 1;
        }
    }

    let response = format!(
        concat!(
            "{{",
            "\"schemaVersion\":{schema_version},",
            "\"ok\":true,",
            "\"operation\":\"render-video-poster\",",
            "\"backend\":\"ffmpeg-cli\",",
            "\"outputPath\":{output_path}",
            "}}"
        ),
        schema_version = SCHEMA_VERSION,
        output_path = json_string(&output.to_string_lossy()),
    );
    let _ = writeln!(stdout, "{response}");
    0
}

fn health_json(vips: &BackendStatus, ffmpeg: &BackendStatus, ffprobe: &BackendStatus) -> String {
    let render_image = vips.is_available();
    let render_video_poster = ffmpeg.is_available();
    let inspect_image = vips.is_available();
    let inspect_video = ffprobe.is_available();
    let mut license_warnings = Vec::new();
    if !vips.is_available() {
        license_warnings.push("libvips runtime is not detected".to_string());
    }
    if !ffmpeg.is_available() {
        license_warnings.push("ffmpeg runtime is not detected".to_string());
    }

    format!(
        concat!(
            "{{",
            "\"schemaVersion\":{schema_version},",
            "\"ok\":true,",
            "\"helper\":{{",
            "\"name\":\"timich-media-helper\",",
            "\"version\":{version},",
            "\"platform\":{platform}",
            "}},",
            "\"capabilities\":{{",
            "\"renderImage\":{render_image},",
            "\"renderVideoPoster\":{render_video_poster},",
            "\"inspectImage\":{inspect_image},",
            "\"inspectVideo\":{inspect_video}",
            "}},",
            "\"image\":{image},",
            "\"video\":{{",
            "\"backend\":\"ffmpeg-cli\",",
            "\"ffmpeg\":{ffmpeg},",
            "\"ffprobe\":{ffprobe},",
            "\"decoders\":[],",
            "\"warnings\":{video_warnings}",
            "}},",
            "\"licenseProfile\":{{",
            "\"ffmpeg\":\"lgpl-decode-only\",",
            "\"libvips\":\"decode-focused\",",
            "\"warnings\":{license_warnings}",
            "}}",
            "}}"
        ),
        schema_version = SCHEMA_VERSION,
        version = json_string(env!("CARGO_PKG_VERSION")),
        platform = json_string(&platform_identifier()),
        render_image = json_bool(render_image),
        render_video_poster = json_bool(render_video_poster),
        inspect_image = json_bool(inspect_image),
        inspect_video = json_bool(inspect_video),
        image = image_json(vips),
        ffmpeg = backend_json(ffmpeg),
        ffprobe = backend_json(ffprobe),
        video_warnings = json_string_array(&combined_warnings(&[ffmpeg, ffprobe])),
        license_warnings = json_string_array(&license_warnings),
    )
}

impl BackendStatus {
    fn is_available(&self) -> bool {
        self.status == "available" && self.path.is_some()
    }
}

fn image_json(vips: &BackendStatus) -> String {
    format!(
        concat!(
            "{{",
            "\"backend\":\"libvips-cli\",",
            "\"runtime\":{runtime},",
            "\"decoders\":[],",
            "\"warnings\":{warnings}",
            "}}"
        ),
        runtime = backend_json(vips),
        warnings = json_string_array(&vips.warnings),
    )
}

fn backend_json(status: &BackendStatus) -> String {
    format!(
        concat!(
            "{{",
            "\"name\":{name},",
            "\"status\":{status},",
            "\"path\":{path},",
            "\"source\":{source},",
            "\"warnings\":{warnings}",
            "}}"
        ),
        name = json_string(status.name),
        status = json_string(status.status),
        path = json_optional_path(status.path.as_deref()),
        source = json_string(status.source),
        warnings = json_string_array(&status.warnings),
    )
}

fn combined_warnings(backends: &[&BackendStatus]) -> Vec<String> {
    let mut warnings = Vec::new();
    for backend in backends {
        warnings.extend(backend.warnings.iter().cloned());
    }
    warnings
}

fn detect_backend(
    name: &'static str,
    binary_name: &'static str,
    env_names: &[&'static str],
    bundled_components: &[&str],
) -> BackendStatus {
    for env_name in env_names {
        if let Some(path) = non_empty_env_path(env_name) {
            if is_executable_file(&path) {
                return BackendStatus {
                    name,
                    status: "available",
                    path: Some(path),
                    source: "env",
                    warnings: Vec::new(),
                };
            }
            return BackendStatus {
                name,
                status: "failed",
                path: Some(path.clone()),
                source: "env",
                warnings: vec![format!("{env_name} points to a missing or non-executable file")],
            };
        }
    }

    for path in bundled_backend_candidates(binary_name, bundled_components) {
        if is_executable_file(&path) {
            return BackendStatus {
                name,
                status: "available",
                path: Some(path),
                source: "bundled",
                warnings: Vec::new(),
            };
        }
    }

    if let Some(path) = find_on_path(binary_name) {
        return BackendStatus {
            name,
            status: "available",
            path: Some(path),
            source: "path",
            warnings: Vec::new(),
        };
    }

    BackendStatus {
        name,
        status: "unavailable",
        path: None,
        source: "none",
        warnings: vec![format!("{binary_name} was not found")],
    }
}

fn non_empty_env_path(name: &str) -> Option<PathBuf> {
    let value = env::var_os(name)?;
    if value.is_empty() {
        return None;
    }
    Some(PathBuf::from(value))
}

fn bundled_backend_candidates(binary_name: &str, bundled_components: &[&str]) -> Vec<PathBuf> {
    let Ok(exe) = env::current_exe() else {
        return Vec::new();
    };
    let Some(exe_dir) = exe.parent() else {
        return Vec::new();
    };

    let mut relative = PathBuf::new();
    for component in bundled_components {
        relative.push(component);
    }
    relative.push(binary_name);

    vec![exe_dir.join(&relative), exe_dir.join("..").join(&relative)]
}

fn find_on_path(binary_name: &str) -> Option<PathBuf> {
    let path_var = env::var_os("PATH")?;
    for dir in env::split_paths(&path_var) {
        let candidate = dir.join(binary_name);
        if is_executable_file(&candidate) {
            return Some(candidate);
        }
    }
    None
}

fn is_executable_file(path: &Path) -> bool {
    let Ok(metadata) = fs::metadata(path) else {
        return false;
    };
    if !metadata.is_file() {
        return false;
    }
    is_executable_metadata(&metadata)
}

#[cfg(unix)]
fn is_executable_metadata(metadata: &fs::Metadata) -> bool {
    use std::os::unix::fs::PermissionsExt;
    metadata.permissions().mode() & 0o111 != 0
}

#[cfg(not(unix))]
fn is_executable_metadata(_metadata: &fs::Metadata) -> bool {
    true
}

fn media_binary_name(name: &'static str) -> &'static str {
    if cfg!(windows) {
        match name {
            "vips" => "vips.exe",
            "ffmpeg" => "ffmpeg.exe",
            "ffprobe" => "ffprobe.exe",
            _ => name,
        }
    } else {
        name
    }
}

fn platform_identifier() -> String {
    format!("{}_{}", env::consts::OS, env::consts::ARCH)
}

fn json_bool(value: bool) -> &'static str {
    if value {
        "true"
    } else {
        "false"
    }
}

fn json_string(value: &str) -> String {
    let mut out = String::with_capacity(value.len() + 2);
    out.push('"');
    for ch in value.chars() {
        match ch {
            '"' => out.push_str("\\\""),
            '\\' => out.push_str("\\\\"),
            '\n' => out.push_str("\\n"),
            '\r' => out.push_str("\\r"),
            '\t' => out.push_str("\\t"),
            ch if ch <= '\u{1f}' => out.push_str(&format!("\\u{:04x}", ch as u32)),
            ch => out.push(ch),
        }
    }
    out.push('"');
    out
}

fn json_optional_path(path: Option<&Path>) -> String {
    match path {
        Some(path) => json_string(&path.to_string_lossy()),
        None => "null".to_string(),
    }
}

fn json_string_array(values: &[String]) -> String {
    let mut out = String::from("[");
    for (index, value) in values.iter().enumerate() {
        if index > 0 {
            out.push(',');
        }
        out.push_str(&json_string(value));
    }
    out.push(']');
    out
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn json_string_escapes_control_characters() {
        assert_eq!(json_string("a\"b\\c\n"), "\"a\\\"b\\\\c\\n\"");
    }

    #[test]
    fn health_json_reports_missing_backends_as_unavailable() {
        let missing = BackendStatus {
            name: "libvips-cli",
            status: "unavailable",
            path: None,
            source: "none",
            warnings: vec!["vips was not found".to_string()],
        };
        let body = health_json(&missing, &missing, &missing);
        assert!(body.contains("\"ok\":true"));
        assert!(body.contains("\"renderImage\":false"));
        assert!(body.contains("\"status\":\"unavailable\""));
    }

    #[test]
    fn failed_backend_path_does_not_enable_capability() {
        let failed = BackendStatus {
            name: "libvips-cli",
            status: "failed",
            path: Some(PathBuf::from("/missing/vips")),
            source: "env",
            warnings: vec!["bad path".to_string()],
        };
        let missing = BackendStatus {
            name: "ffmpeg-cli",
            status: "unavailable",
            path: None,
            source: "none",
            warnings: Vec::new(),
        };
        let body = health_json(&failed, &missing, &missing);
        assert!(body.contains("\"renderImage\":false"));
        assert!(body.contains("\"status\":\"failed\""));
    }

    #[test]
    fn platform_identifier_is_go_style_pair() {
        let platform = platform_identifier();
        assert!(platform.contains('_'));
        assert!(!platform.starts_with('_'));
        assert!(!platform.ends_with('_'));
    }
}
