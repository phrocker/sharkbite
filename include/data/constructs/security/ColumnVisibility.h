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

#ifndef COLUMNVISIBILITY_H
#define COLUMNVISIBILITY_H 1

#pragma once

#include <string>
#include <set>

#include <vector>

#include "../../exceptions/ClientException.h"
#include "data/exceptions/IllegalArgumentException.h"
#include "VisibilityNode.h"
#include "Authorizations.h"

namespace cclient {
namespace data {
namespace security {

class ColumnVisibility {

 public:
  explicit ColumnVisibility(const std::string&);

  static VisibilityNode normalize(VisibilityNode &root, const std::string &expression);

  std::string flatten();

  VisibilityNode* getTree() {
    return &node;
  }

 protected:

  void astToSTring(VisibilityNode &node, std::string expression, std::string &out);

  VisibilityNode processTerm(size_t start, size_t end, VisibilityNode &expr, std::string &expression);

  VisibilityNode parse_(std::string &expression);

  VisibilityNode node;
  std::string expression;
  size_t index;
  size_t parens;
};
}
}
}
#endif /* COLUMNVISIBILITY_H */
