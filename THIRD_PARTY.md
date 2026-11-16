# Third-party notices

The Mixtape executable itself is built only with the Go standard library and locally authored HTML/CSS/JS. The audio helpers are separately downloaded executables and are not part of the lightweight ZIP. Mixtape v1.4 can additionally run yt-dlp's own official stable-channel updater while the queue is idle; the initially pinned downloader may therefore change after first launch.

| Project | Purpose | Licence / source information |
|---|---|---|
| yt-dlp | Media downloading and searching | https://github.com/yt-dlp/yt-dlp/blob/master/LICENSE ; standalone EXE includes further third-party licence notices |
| FFmpeg | MP3 conversion, cover embedding, ID3 tags | https://ffmpeg.org/legal.html ; licensing depends on codecs and flags in the exact build |
| Deno | Required JavaScript runtime | https://github.com/denoland/deno/blob/main/LICENSE.md |
| yt-dlp-ejs | YouTube JavaScript challenge solver | https://github.com/yt-dlp/ejs ; licensed with bundled attributions |
| MusicBrainz | Artist/album metadata | https://musicbrainz.org/doc/MusicBrainz_API |
| Cover Art Archive | Cover artwork metadata and images | https://musicbrainz.org/doc/Cover_Art_Archive/API ; artwork copyright remains with its owners |

If distributing a version that **bundles** helper executable binaries (rather than downloading them when launched), review each component's applicable licence and include required notices and source-availability information. FFmpeg builds with GPL codecs have corresponding GPL obligations. See the providers' websites for current binaries and source code.
