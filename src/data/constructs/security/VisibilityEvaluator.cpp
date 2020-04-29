/*
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

#include "data/constructs/security/VisibilityEvaluator.h"
#include <string>
#include <set>
#include <iostream>

namespace cclient {
namespace data {
namespace security {



bool VisibilityEvaluator::evaluate(const std::string &expression, const VisibilityNode &root) {
   if (expression.length() == 0)
     return true;
   switch (root.getType()) {
     case TERM:
       return auths.contains(root.getTerm(expression).getTerm());
     case AND:
       if (root.getSize() < 2)
         throw cclient::exceptions::VisibilityParseException("AND has less than 2 children", expression, root.getTermStart());
       for (const auto &child : root.getChildren()) {
         if (!evaluate(expression, child))
           return false;
       }
       return true;
     case OR:
       if (root.getSize() < 2)
         throw cclient::exceptions::VisibilityParseException("OR has less than 2 children", expression, root.getTermStart());
       for (const auto &child : root.getChildren()) {
         if (evaluate(expression, child))
           return true;
       }
       return false;
     default:
       throw cclient::exceptions::VisibilityParseException("No such node type", expression, root.getTermStart());
   }
 }

}
}
}
