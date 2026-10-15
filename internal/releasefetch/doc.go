// Package releasefetch is what `berth teardown pin --release` and `--latest`
// need to turn a published release into bytes worth pinning (#85,
// docs/design/teardown.md): a bounded HTTPS fetcher, the release's URLs,
// checksums.txt parsing, the cosign policy and its runner, and archive
// extraction.
//
// Nothing here writes to the control home or reads the environment. The
// caller builds the *http.Client (cli.MainProcess), names the process
// environment cosign is to run in, and decides what to do with the verified
// bytes. Every input from the network is untrusted until cosign has verified
// checksums.txt; the extracted binary is untrusted until its digest has been
// matched against that file.
package releasefetch
