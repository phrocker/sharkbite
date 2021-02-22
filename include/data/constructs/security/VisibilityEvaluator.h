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

#ifndef VISIBILITYEVALUATOR_H
#define VISIBILITYEVALUATOR_H 1

#pragma once

#include <string>
#include <set>

#include <vector>

#include "../../exceptions/ClientException.h"
#include "utils/MemoryUtils.h"
#include "data/constructs/security/Authorizations.h"
#include "VisibilityNode.h"
#include "data/exceptions/VisibilityParseException.h"
#include "ColumnVisibility.h"

namespace cclient {
namespace data {
namespace security {

class VisibilityEvaluator {

  cclient::data::security::Authorizations auths;
  // not intended to be thread safe.
  std::map<std::string, bool> resultMap;
 public:
  explicit VisibilityEvaluator(const cclient::data::security::Authorizations &auths)
      :
      auths(auths) {

  }

  VisibilityEvaluator() {
  }

  void setVisibility(const cclient::data::security::Authorizations &at) {
    auths = at;
  }

  bool evaluate(const std::vector<char> &expr) {
    return evaluate(std::string(expr.data(), expr.size()));
  }

  bool evaluate(const std::string &expr) {
    auto cacheLookup = resultMap.find(expr);
    if (cacheLookup != std::end(resultMap)) {
      return cacheLookup->second;
    }
    if (expr.empty() && !auths.empty()){
      return false;
    }
    auto cv = ColumnVisibility(expr);
    auto tree = cv.getTree();
    auto result = evaluate(expr, *tree);
    resultMap[expr] = result;
    return result;
  }

  bool evaluate(const std::string &expression, const VisibilityNode &root);

 protected:

};

}
}
}
#endif /* VISIBILITYEVALUATOR_H */
