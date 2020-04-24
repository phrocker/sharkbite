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

#include "data/constructs/security/ColumnVisibility.h"
#include "data/constructs/security/VisibilityNode.h"
#include <string>
#include <set>

namespace cclient {
namespace data {
namespace security {

ColumnVisibility::ColumnVisibility(const std::string &expression)
    :
    node(nullptr),
    expression(expression) {

}

VisibilityNode ColumnVisibility::normalize(VisibilityNode &root, const std::string &expression) {
  if (root.type != NodeType::TERM) {
    std::set<VisibilityNode, ExpressionASTComparator> rolledUp;

    auto children = root.getChildrenRef();
    auto it = children->begin();

    while (it != children->end()) {
      // copy the current iterator then increment it
      auto nxt = it++;
      VisibilityNode c = normalize(*nxt, expression);
      if (c.getType() == root.getType()) {
        rolledUp.insert(children->begin(), children->end());
        children->erase(nxt);
      }

    }

    rolledUp.insert(children->begin(), children->end());
    //rolledUp.addAll(root.children);
    children->clear();
    children->insert(children->begin(), rolledUp.begin(), rolledUp.end());

    if (children->size() == 1) {
      return children->at(0);
    }
  }
  return root;
}

}
}
}
