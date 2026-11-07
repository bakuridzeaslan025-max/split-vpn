// Package bind is never built. It holds the module gomobile runs from, so that
// tunnel and vkfallback land in one AAR: two Go runtimes cannot share an APK.
package bind

import (
	_ "splitvpn.local/vkfallback"
	_ "tunnel"
)
