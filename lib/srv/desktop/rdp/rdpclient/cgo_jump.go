package rdpclient

/*
#include <librdprs.h>
#include <stdint.h>

extern void handleBitmapJump(int64_t, struct Bitmap);

void handleBitmap_cgo(int64_t cp, struct Bitmap cb) {
	handleBitmapJump(cp, cb);
}
*/
import "C"
