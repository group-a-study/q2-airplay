# q2-airplay

AirPlay for [Q2 Pod](https://github.com/DiamondBond/q2-pod) on the Shanling Q2: what the Q2 plays, on an
AirPlay speaker on the same Wi-Fi. That includes Sonos, HomePod, Apple TV and AirPort Express, and other
AirPlay 1 and 2 speakers.

It needs a Q2 Pod build with AirPlay: the Now Playing button, the audio tap in the player, and the
lyrics and volume handling. For now those are only in group-a-study/q2-pod's test builds.

## Use

1. Install it from **System settings → Apps → AirPlay**.
2. On **Now Playing**, tap the AirPlay button left of shuffle, or pick **AirPlay** on the ⋯ page.
3. Pick a speaker. The Q2 goes quiet, and about a second later the speaker plays what the Q2 plays.
4. To play on the Q2 again, pick **This Q2**.

While a speaker plays:

- **Volume:** the wheel and keys set the speaker's volume. It's saved apart from the Q2's own, so your
  earphones' level comes back when you switch back.
- **Delay:** the speaker plays a fixed delay behind the Q2, 1 s by default. It's set on the AirPlay page
  (0.75–3 s); more rides out weak Wi-Fi.
- **Pause, skip and seek:** the speaker does the same, the delay later. A pause stops it exactly where
  the Q2 paused, with nothing lost.
- **Lyrics:** they follow the speaker, not the Q2.
- **The speaker's display:** shows the song's title, artist, album and cover. The cover comes from a
  `cover.jpg` (or `folder.jpg`, `front.jpg`…) beside the song, else the picture in the file.
- **The Q2's EQ:** it's for your earphones, so it's left out. The speaker gets the song as it is.
- **Sound quality:** it goes out as lossless ALAC at 44.1 kHz/16-bit, what AirPlay carries.
  16-bit/44.1 kHz songs arrive bit for bit. Hi-res ones are converted on the Q2.

What can't be played on a speaker: DSD (it goes to the Q2's DAC untouched). Spotify, Internet Radio and
Videos have their own players, which this doesn't reach.

## How it works

- **The audio:** Q2 Pod's player (hciplayer) has a filter that puts what plays into a ring in
  `/tmp/q2cast`, at 44.1 kHz, while this app reads it. It also marks seeks and skips, and silences the
  Q2's own output once a speaker plays.
- **The app** (`cmd/q2-airplay`):
  - finds speakers over mDNS (`_airplay._tcp`, `_raop._tcp`);
  - draws the speaker page over Q2SPOT/1, as Q2 Pod's other card apps do;
  - hands the audio to cliairplay, a fixed delay behind the tap (`cast.go`).
- **The sender:** cliairplay is [Music Assistant's AirPlay sender](https://github.com/music-assistant/airplay-cli),
  built for the Q2 by `contrib/shanlingq2/build-cliairplay.sh`. It does the protocol work: pairing,
  PTP timing, the buffered stream Sonos speakers take, and the speaker's now-playing.
- **AirPlay 2 or 1:** AirPlay 2 comes first. If it can't connect, the app tries the speaker over AirPlay 1.
- **Battery:** audio is sent in 200 ms bursts, so the Wi-Fi can doze between them. Paused, the stream
  stops; nothing is looked for or sent until the page opens or a speaker is chosen. The app ends by
  itself once unused for 10 minutes.

## Build

    contrib/shanlingq2/build.sh 'Q2 Firmware V1.32.zip'

This needs Go 1.22 or later, plus what `build-cliairplay.sh` lists (clang, lld, git, curl, unsquashfs,
python3). cliairplay links against the Q2's own glibc, libstdc++ and libcrypto, taken from Shanling's
firmware ZIP, which isn't public. So releases are built locally and published by
`contrib/shanlingq2/release.sh`.

Tests: `go test ./...` and `python3 contrib/shanlingq2/test_launcher.py`. `TestEngine` runs the casting
engine against a simulated player filter and a stand-in cliairplay.

## Licence

GPL-3.0-or-later (`LICENSE`). cliairplay is GPL-3.0; the zip's `.airplay/SOURCES` names the sources each
build used.
