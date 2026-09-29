# Kotlin objects handed to Go (tunnel.Host, tunnel.Logger) are called back
# through the Go-side interface, which the AAR keeps; nothing extra needed.

# Shrink only. Obfuscation buys nothing (the masking is on the wire, not in
# the code) and makes stack traces in user logs unreadable while we test.
# Turn it back on when leaving internal testing.
-dontobfuscate
-keepattributes SourceFile,LineNumberTable
