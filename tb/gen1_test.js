var testPlatforms = [
    "linux/amd64",
    "linux/amd64/race",
    "linux/386",
];

var buildPlatforms = [
    "darwin/arm64",
];

// For plan9, don't test+vet all packages. Just make sure we keep compiling
// the daemon+CLI.
addTask("build", "plan9/amd64", "./cmd/tailscaled")
addTask("build", "plan9/amd64", "./cmd/tailscale")

testPlatforms.forEach(function(plat) { 
    addTask("build", plat, "./cmd/...")
    getPackages(plat).forEach(function(pkg) {
        if (pkg.hasTests) {
            addTask("test", plat, pkg.importPath);
        }
    })
})

buildPlatforms.forEach(function(plat) {
    addTask("build", plat, "./cmd/...")
    addTask("test-compile", plat, "./...")
})

var staticcheckPlatforms = [
    "linux/amd64",
    "darwin/amd64",
    "windows/amd64",
    "windows/386",
];
staticcheckPlatforms.forEach(function(plat) {
    getPackages(plat).forEach(function(pkg) {
        if (pkg.importPath.indexOf("/tempfork/") == -1) {
            addTask("staticcheck", plat, pkg.importPath)
        }
    })
})