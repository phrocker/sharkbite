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

#include <iostream>
#include <set>
#include <string>

#include "data/constructs/security/VisibilityNode.h"

namespace cclient {
namespace data {
namespace security {

ColumnVisibility::ColumnVisibility(const std::string &expr)
    : expression(expr), index(0) {
  node = parse_(expression);
}

std::string ColumnVisibility::flatten() {
  VisibilityNode normRoot = normalize(node, expression);
  std::string out;
  astToSTring(normRoot, expression, out);
  return out;
}

void ColumnVisibility::astToSTring(VisibilityNode &root, std::string expression,
                                   std::string &out) {
  if (root.getType() == NodeType::TERM) {
    out += std::string(expression, root.start, root.getLength());
  } else {
    std::string sep = "";
    for (VisibilityNode c : root.getChildren()) {
      out += sep;
      bool parens =
          (c.getType() != NodeType::TERM && root.getType() != c.getType());
      if (parens) out += "(";
      astToSTring(c, expression, out);
      if (parens) out += ")";
      sep = root.getType() == NodeType::AND ? "&" : "|";
    }
  }
}

VisibilityNode ColumnVisibility::processTerm(size_t start, size_t end,
                                             VisibilityNode &expr,
                                             std::string &expression) {
  if (start != end) {
    if (!expr.empty()) {
      throw cclient::exceptions::IllegalArgumentException(
          "expression needs | or &");
    }
    return VisibilityNode(&expression, start, end);
  }
  if (expr.empty()) {
    throw cclient::exceptions::IllegalArgumentException("empty term");
  }
  return expr;
}

VisibilityNode ColumnVisibility::parse_(std::string &expression) {
  VisibilityNode result;
  VisibilityNode expr;
  size_t wholeTermStart = index;
  size_t subtermStart = index;
  bool subtermComplete = false;

  while (index < expression.length()) {
    switch (expression[index++]) {
      case '&': {
        expr = processTerm(subtermStart, index - 1, expr, expression);
        if (!result.empty()) {
          if (result.getType() != NodeType::AND) {
            throw cclient::exceptions::IllegalArgumentException(
                "cannot mix & and |");
          }
        } else {
          result = VisibilityNode(&expression, NodeType::AND, wholeTermStart);
        }
        result.add(std::move(expr));
        subtermStart = index;
        subtermComplete = false;
        break;
      }
      case '|': {
        expr = processTerm(subtermStart, index - 1, expr, expression);
        if (!result.empty()) {
          if (result.getType() != NodeType::OR) {
            throw cclient::exceptions::IllegalArgumentException(
                "cannot mix | and &");
          }
        } else {
          result = VisibilityNode(&expression, NodeType::OR, wholeTermStart);
        }
        result.add(std::move(expr));
        subtermStart = index;
        subtermComplete = false;
        break;
      }
      case '(': {
        parens++;
        if (subtermStart != index - 1 || !expr.empty()) {
          throw cclient::exceptions::IllegalArgumentException(
              "expression needs & or |");
        }
        expr = parse_(expression);
        subtermStart = index;
        subtermComplete = false;
        break;
      }
      case ')': {
        parens--;
        auto child = processTerm(subtermStart, index - 1, expr, expression);
        if (child.empty() && result.empty()) {
          throw cclient::exceptions::IllegalArgumentException(
              "empty expression not allowed");
        }
        if (result.empty()) return child;
        if (result.getType() == child.getType())
          for (const auto &c : child.getChildren()) result.add(c);
        else
          result.add(child);
        result.end = index - 1;
        return result;
      }
      case '"': {
        if (subtermStart != index - 1) {
          throw cclient::exceptions::IllegalArgumentException(
              "expression needs & or |");
        }

        while (index < expression.length() && expression[index] != '"') {
          if (expression[index] == '\\') {
            index++;
            if (index == expression.length() ||
                (expression[index] != '\\' && expression[index] != '"')) {
              throw cclient::exceptions::IllegalArgumentException(
                  "invalid escaping within quotes");
            }
          }
          index++;
        }

        if (index == expression.length()) {
          throw cclient::exceptions::IllegalArgumentException("unclosed quote");
        }

        if (subtermStart + 1 == index) {
          throw cclient::exceptions::IllegalArgumentException("empty term");
        }

        index++;

        subtermComplete = true;

        break;
      }
      default:
        if (subtermComplete) {
          throw cclient::exceptions::IllegalArgumentException(
              "expression needs & or |");
        }

        char c = expression[index - 1];
        if (!Authorizations::isValidAuthCharacter(c)) {
          throw cclient::exceptions::IllegalArgumentException(
              "bad character (" + std::to_string(c) + ")");
        }
    }
  }
  auto child = processTerm(subtermStart, index, expr, expression);
  if (!result.empty()) {
    result.add(child);
    result.end = index;
  } else
    result = child;
  if (result.getType() != NodeType::TERM)
    if (result.getSize() < 2) {
      throw cclient::exceptions::IllegalArgumentException("missing term");
    }
  return result;
}

VisibilityNode ColumnVisibility::normalize(VisibilityNode &root,
                                           const std::string &expression) {
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
    // rolledUp.addAll(root.children);
    children->clear();
    children->insert(children->begin(), rolledUp.begin(), rolledUp.end());

    if (children->size() == 1) {
      return children->at(0);
    }
  }
  return root;
}

}  // namespace security
}  // namespace data
}  // namespace cclient
