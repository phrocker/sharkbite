// Licensed under the Apache License, Version 2.0. See LICENSE and NOTICE.
package decision

import "github.com/phrocker/shoal-oss/pkg/decision/class"

// DecisionClass is the decision class, defined in the leaf package
// pkg/decision/class so the wire contracts can carry it without linking
// this package. See class.DecisionClass.
type DecisionClass = class.DecisionClass
