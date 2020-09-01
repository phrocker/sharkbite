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
#ifndef TABLETLOCATION_H_
#define TABLETLOCATION_H_

#include <cstdlib>
#include <cstdio>
#include <string>

#include <map>

#include "../constructs/KeyExtent.h"

namespace cclient
{
  namespace data
  {

    class TabletLocation
    {
    public:
      TabletLocation();
      TabletLocation(std::shared_ptr<KeyExtent> extent, std::string loc)
          : tablet_extent(extent),
            tablet_location(loc),
            port(0)
      {
      }

      TabletLocation(std::shared_ptr<KeyExtent> extent, std::string loc, std::string sesh)
          : tablet_extent(extent),
            tablet_location(loc),
            session(sesh)
      {
        std::vector<std::string> tokens = split(tablet_location, ':');
        if (!IsEmpty(&tokens))
        {
          server = tokens.at(0);
          port = atoi(tokens.at(1).c_str());
        }
      }

      explicit TabletLocation(TabletLocation *copy)
          : tablet_extent(copy->getExtent()),
            tablet_location(copy->getLocation()),
            session(copy->getSession())
      {
        std::vector<std::string> tokens = split(tablet_location, ':');
        if (!IsEmpty(&tokens))
        {
          server = tokens.at(0);
          port = atoi(tokens.at(1).c_str());
        }
      }

      std::shared_ptr<KeyExtent> getExtent()
      {
        return tablet_extent;
      }

      void setExtent(const std::shared_ptr<KeyExtent> &extent)
      {
        tablet_extent = extent;
      }

      std::string getLocation()
      {
        return tablet_location;
      }

      std::string getServer()
      {
        return server;
      }

      std::string getSession()
      {
        return session;
      }

      int getPort()
      {
        return port;
      }

      bool operator!=(std::nullptr_t type)
      {
        return port > 0 && !tablet_location.empty();
      }

      bool operator==(std::nullptr_t type)
      {
        return port == 0 && tablet_location.empty() && server.empty();
      }

      TabletLocation &
      operator=(const TabletLocation &te)
      {
        tablet_extent = te.tablet_extent;
        tablet_location = te.tablet_location;
        server = te.server;
        port = te.port;
        session = te.session;
        return *this;
      }

      virtual ~TabletLocation();

    protected:
      std::shared_ptr<KeyExtent> tablet_extent;
      std::string tablet_location;
      std::string server;
      int port;
      std::string session;
    };

  } /* namespace data */
} /* namespace cclient */
#endif /* TABLETLOCATION_H_ */
