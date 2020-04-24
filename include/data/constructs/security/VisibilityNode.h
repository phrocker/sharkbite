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

#ifndef VISIBILITYNODE_H
#define VISIBILITYNODE_H 1

#pragma once

#include <string>
#include <set>

#include <vector>

#include "../../exceptions/ClientException.h"
#include "utils/MemoryUtils.h"

namespace cclient {
namespace data {
namespace security {

enum NodeType {
  // 0 , 1, 2, 3
  EMPTY = 0,
  TERM,
  OR,
  AND
};

class NodeExpression {
 public:

  explicit NodeExpression(char *buf, size_t offset, size_t size)
      :
      buffer(buf),
      offset(offset),
      size(size) {

  }

  char* getBuffer() const {
    return buffer;
  }

  size_t getOffset() const {
    return offset;
  }

  size_t getSize() const {
    return size;
  }

 private:

  char *buffer;
  size_t offset;
  size_t size;
};

class VisibilityNode {
 public:
  /**
   * An empty list of nodes.
   */
  std::string *expression;
  NodeType type;
  int start;
  int end;
  std::vector<VisibilityNode> children;

  explicit VisibilityNode(std::string *expr, NodeType type, int start)
      :
      expression(expr),
      type(type),
      start(start),
      end(start + 1) {
  }

  explicit VisibilityNode(std::string *expr, int start, int end)
      :
      expression(expr),
      type(NodeType::TERM),
      start(start),
      end(end) {
  }

  const char* getExpression() const {
    return expression->data();
  }

  void add(const VisibilityNode &child) {
    children.emplace_back(child);
  }

  NodeType getType() const {
    return type;
  }

  std::vector<VisibilityNode>* getChildrenRef() {
    return &children;
  }

  std::vector<VisibilityNode> getChildren() const {
    return children;
  }

  size_t getSize() const {
    return children.size();
  }

  int getLength() const {
    return start - end;
  }

  int getTermStart() const {
    return start;
  }

  int getTermEnd() const {
    return end;
  }

  NodeExpression getTerm(char *expression) {
    if (type != NodeType::TERM)
      throw std::runtime_error("Invalid node type");

    if (expression[start] == '"') {
      // its a quoted term
      int qStart = start + 1;
      int qEnd = end - 1;
      return NodeExpression(expression, qStart, qEnd - qStart);
    }
    return NodeExpression(expression, start, end - start);
  }
};

struct ExpressionASTComparator {
  bool operator()(const VisibilityNode &lhs, const VisibilityNode &rhs) {
    auto diff = static_cast<int>(lhs.getType()) - static_cast<int>(rhs.getType());
    if (diff > 0)
      return false;
    if (diff < 0)
      return true;
    switch (lhs.getType()) {
      case EMPTY:
        return false;  // All empty nodes are the same
      case TERM: {
        auto termstr = lhs.getExpression();
        return cclient::MemoryUtils::compare(termstr + lhs.getTermStart(), lhs.getLength(), termstr + rhs.getTermStart(), rhs.getLength()) < 0;
      }
      case OR:
      case AND: {
        diff = lhs.getSize() - rhs.getSize();
        if (diff > 0)
          return false;
        if (diff < 0)
          return true;
        auto lhs_children = lhs.getChildren();
        auto rhs_children = rhs.getChildren();
        for (size_t i = 0; i < lhs_children.size(); i++) {
          diff = operator()(lhs_children.at(i), rhs.children.at(i));
          if (diff > 0)
            return false;
          if (diff < 0)
            return true;
        }
      }
    }
    return false;
  }
};

}
}
}
#endif /* VISIBILITYNODE_H */
