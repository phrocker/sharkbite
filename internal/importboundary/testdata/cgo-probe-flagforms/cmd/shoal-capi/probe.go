// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.

package main

/*
#cgo CFLAGS: @${SRCDIR}/flags.rsp -Wp,-include,${SRCDIR}/x.h -Xpreprocessor -iquote${SRCDIR}
#cgo CPPFLAGS: -DOK=1
*/
import "C"
