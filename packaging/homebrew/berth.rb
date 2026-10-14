class Berth < Formula
  desc "Give every Git worktree a durable identity, an isolated home, and a routed URL"
  homepage "https://github.com/sofired/berth"
  url "https://github.com/sofired/berth/releases/download/v0.0.0/berth-0.0.0.tar.gz"
  sha256 "0000000000000000000000000000000000000000000000000000000000000000"
  license "Apache-2.0"

  head "https://github.com/sofired/berth.git", branch: "main"

  # go is build-only: the tap's CI builds and publishes bottles for this
  # formula, so a normal `brew install` downloads a prebuilt binary rather than
  # compiling.
  depends_on "go" => :build

  def install
    # Releases never link cgo.
    ENV["CGO_ENABLED"] = "0"
    ldflags = "-s -w -X github.com/sofired/berth/internal/brand.Version=#{version}"
    system "go", "build", *std_go_args(ldflags:), "./cmd/berth"
    generate_completions_from_executable(bin/"berth", "completion", shells: [:bash, :zsh, :fish])
  end

  def caveats
    <<~EOS
      berth needs Git, Docker and Docker Compose at run time.
      `berth doctor` checks all three.
    EOS
  end

  test do
    assert_match version.to_s, shell_output("#{bin}/berth --version")
    assert_match "_berth", shell_output("#{bin}/berth completion bash")
  end
end
