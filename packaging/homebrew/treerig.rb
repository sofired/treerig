class Treerig < Formula
  desc "Give every Git worktree a durable identity, an isolated home, and a routed URL"
  homepage "https://github.com/sofired/treerig"
  url "https://github.com/sofired/treerig/releases/download/v0.0.0/treerig-0.0.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "Apache-2.0"

  head "https://github.com/sofired/treerig.git", branch: "main"

  # go is build-only: the tap's CI builds and publishes bottles for this
  # formula, so a normal `brew install` downloads a prebuilt binary rather than
  # compiling.
  depends_on "go" => :build

  def install
    # Releases never link cgo.
    ENV["CGO_ENABLED"] = "0"
    ldflags = "-s -w -X github.com/sofired/treerig/internal/brand.Version=#{version}"
    system "go", "build", *std_go_args(ldflags:), "./cmd/treerig"
    generate_completions_from_executable(bin/"treerig", "completion", shells: [:bash, :zsh, :fish])
    # `trig` is a short alias: the same binary under a second name, which
    # prints and completes as `trig` when run that way (brand.CLIName follows
    # argv[0]). Its completions come from running it under that name.
    bin.install_symlink "treerig" => "trig"
    generate_completions_from_executable(bin/"trig", "completion", shells: [:bash, :zsh, :fish], base_name: "trig")
  end

  def caveats
    <<~EOS
      treerig needs Git, Docker and Docker Compose at run time.
      `treerig doctor` checks all three.

      `trig` is the same program under a shorter name.
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/treerig --version")
    assert_match "_treerig", shell_output("#{bin}/treerig completion bash")
    assert_match version.to_s, shell_output("#{bin}/trig --version")
    assert_match "_trig", shell_output("#{bin}/trig completion bash")
    assert_predicate bin/"trig", :symlink?
  end
end
