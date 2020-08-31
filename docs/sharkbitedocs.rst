Sharkbite Documentation
***********************
.. raw:: html

  <embed>

  <p>Accumulo connector plugin</p>
  <dl class="py class">
  <dt id="pysharkbite.AccumuloConnector">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">AccumuloConnector</code><a class="headerlink" href="#pysharkbite.AccumuloConnector" title="Permalink to this definition">¶</a></dt>
  <dd><p>Accumulo connector</p>
  <dl class="py method">
  <dt id="pysharkbite.AccumuloConnector.getStatistics">
  <code class="sig-name descname">getStatistics</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloConnector" title="pysharkbite.AccumuloConnector">pysharkbite.AccumuloConnector</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.AccumuloInfo" title="pysharkbite.AccumuloInfo">pysharkbite.AccumuloInfo</a><a class="headerlink" href="#pysharkbite.AccumuloConnector.getStatistics" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns Statistics for the accumulo connector</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloConnector.namespaceOps">
  <code class="sig-name descname">namespaceOps</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloConnector" title="pysharkbite.AccumuloConnector">pysharkbite.AccumuloConnector</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.AccumuloNamespaceOperations" title="pysharkbite.AccumuloNamespaceOperations">pysharkbite.AccumuloNamespaceOperations</a><a class="headerlink" href="#pysharkbite.AccumuloConnector.namespaceOps" title="Permalink to this definition">¶</a></dt>
  <dd><p>Allows the user to perform namespace operations</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloConnector.securityOps">
  <code class="sig-name descname">securityOps</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloConnector" title="pysharkbite.AccumuloConnector">pysharkbite.AccumuloConnector</a></span></em><span class="sig-paren">)</span> &#x2192; interconnect::PythonSecurityOperations<a class="headerlink" href="#pysharkbite.AccumuloConnector.securityOps" title="Permalink to this definition">¶</a></dt>
  <dd><p>Return the security operations object</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloConnector.tableOps">
  <code class="sig-name descname">tableOps</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloConnector" title="pysharkbite.AccumuloConnector">pysharkbite.AccumuloConnector</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.AccumuloTableOperations" title="pysharkbite.AccumuloTableOperations">pysharkbite.AccumuloTableOperations</a><a class="headerlink" href="#pysharkbite.AccumuloConnector.tableOps" title="Permalink to this definition">¶</a></dt>
  <dd><p>Return the table operations object</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.AccumuloInfo">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">AccumuloInfo</code><a class="headerlink" href="#pysharkbite.AccumuloInfo" title="Permalink to this definition">¶</a></dt>
  <dd><dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.bad_servers">
  <em class="property">property </em><code class="sig-name descname">bad_servers</code><a class="headerlink" href="#pysharkbite.AccumuloInfo.bad_servers" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.dead_servers">
  <em class="property">property </em><code class="sig-name descname">dead_servers</code><a class="headerlink" href="#pysharkbite.AccumuloInfo.dead_servers" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.getBadTabletServers">
  <code class="sig-name descname">getBadTabletServers</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloInfo" title="pysharkbite.AccumuloInfo">pysharkbite.AccumuloInfo</a></span></em><span class="sig-paren">)</span> &#x2192; Dict<span class="p">[</span>str<span class="p">, </span>int<span class="p">]</span><a class="headerlink" href="#pysharkbite.AccumuloInfo.getBadTabletServers" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets a mapping of bad tablet servers.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.getDeadServers">
  <code class="sig-name descname">getDeadServers</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloInfo" title="pysharkbite.AccumuloInfo">pysharkbite.AccumuloInfo</a></span></em><span class="sig-paren">)</span> &#x2192; List<span class="p">[</span><a class="reference internal" href="#pysharkbite.DeadServer" title="pysharkbite.DeadServer">pysharkbite.DeadServer</a><span class="p">]</span><a class="headerlink" href="#pysharkbite.AccumuloInfo.getDeadServers" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns a list of dead tablet servers.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.getGoalState">
  <code class="sig-name descname">getGoalState</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloInfo" title="pysharkbite.AccumuloInfo">pysharkbite.AccumuloInfo</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.CoordinatorGoalState" title="pysharkbite.CoordinatorGoalState">pysharkbite.CoordinatorGoalState</a><a class="headerlink" href="#pysharkbite.AccumuloInfo.getGoalState" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns the goal state of the cluster.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.getServerShuttingDown">
  <code class="sig-name descname">getServerShuttingDown</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloInfo" title="pysharkbite.AccumuloInfo">pysharkbite.AccumuloInfo</a></span></em><span class="sig-paren">)</span> &#x2192; Set<span class="p">[</span>str<span class="p">]</span><a class="headerlink" href="#pysharkbite.AccumuloInfo.getServerShuttingDown" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns the set of servers shutting down.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.getState">
  <code class="sig-name descname">getState</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloInfo" title="pysharkbite.AccumuloInfo">pysharkbite.AccumuloInfo</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.CoordinatorState" title="pysharkbite.CoordinatorState">pysharkbite.CoordinatorState</a><a class="headerlink" href="#pysharkbite.AccumuloInfo.getState" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the state of the accumulo cluster.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.getTableMap">
  <code class="sig-name descname">getTableMap</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloInfo" title="pysharkbite.AccumuloInfo">pysharkbite.AccumuloInfo</a></span></em><span class="sig-paren">)</span> &#x2192; Dict<span class="p">[</span>str<span class="p">, </span><a class="reference internal" href="#pysharkbite.TableInfo" title="pysharkbite.TableInfo">pysharkbite.TableInfo</a><span class="p">]</span><a class="headerlink" href="#pysharkbite.AccumuloInfo.getTableMap" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the Table map for the cluster.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.getTabletServerInfo">
  <code class="sig-name descname">getTabletServerInfo</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloInfo" title="pysharkbite.AccumuloInfo">pysharkbite.AccumuloInfo</a></span></em><span class="sig-paren">)</span> &#x2192; List<span class="p">[</span><a class="reference internal" href="#pysharkbite.TabletServerStatus" title="pysharkbite.TabletServerStatus">pysharkbite.TabletServerStatus</a><span class="p">]</span><a class="headerlink" href="#pysharkbite.AccumuloInfo.getTabletServerInfo" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets tablet server Info</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.getUnassignedTablets">
  <code class="sig-name descname">getUnassignedTablets</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloInfo" title="pysharkbite.AccumuloInfo">pysharkbite.AccumuloInfo</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.AccumuloInfo.getUnassignedTablets" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the unassigned tablets</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.goal_state">
  <em class="property">property </em><code class="sig-name descname">goal_state</code><a class="headerlink" href="#pysharkbite.AccumuloInfo.goal_state" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.servs_shutting_down">
  <em class="property">property </em><code class="sig-name descname">servs_shutting_down</code><a class="headerlink" href="#pysharkbite.AccumuloInfo.servs_shutting_down" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.state">
  <em class="property">property </em><code class="sig-name descname">state</code><a class="headerlink" href="#pysharkbite.AccumuloInfo.state" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.table_map">
  <em class="property">property </em><code class="sig-name descname">table_map</code><a class="headerlink" href="#pysharkbite.AccumuloInfo.table_map" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.tablet_server_info">
  <em class="property">property </em><code class="sig-name descname">tablet_server_info</code><a class="headerlink" href="#pysharkbite.AccumuloInfo.tablet_server_info" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloInfo.unassigned_tablets">
  <em class="property">property </em><code class="sig-name descname">unassigned_tablets</code><a class="headerlink" href="#pysharkbite.AccumuloInfo.unassigned_tablets" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.AccumuloNamespaceOperations">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">AccumuloNamespaceOperations</code><a class="headerlink" href="#pysharkbite.AccumuloNamespaceOperations" title="Permalink to this definition">¶</a></dt>
  <dd><p>Accumulo namespace operations. Should be accessed through ‘AccumuloConnector’</p>
  <dl class="py method">
  <dt id="pysharkbite.AccumuloNamespaceOperations.create">
  <code class="sig-name descname">create</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloNamespaceOperations" title="pysharkbite.AccumuloNamespaceOperations">pysharkbite.AccumuloNamespaceOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.AccumuloNamespaceOperations.create" title="Permalink to this definition">¶</a></dt>
  <dd><p>Creates the namespace</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloNamespaceOperations.exists">
  <code class="sig-name descname">exists</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloNamespaceOperations" title="pysharkbite.AccumuloNamespaceOperations">pysharkbite.AccumuloNamespaceOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.AccumuloNamespaceOperations.exists" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns true if the namespace exists</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloNamespaceOperations.list">
  <code class="sig-name descname">list</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloNamespaceOperations" title="pysharkbite.AccumuloNamespaceOperations">pysharkbite.AccumuloNamespaceOperations</a></span></em><span class="sig-paren">)</span> &#x2192; List<span class="p">[</span>str<span class="p">]</span><a class="headerlink" href="#pysharkbite.AccumuloNamespaceOperations.list" title="Permalink to this definition">¶</a></dt>
  <dd><p>Lists namespaces within this Accumulo instance</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloNamespaceOperations.remove">
  <code class="sig-name descname">remove</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloNamespaceOperations" title="pysharkbite.AccumuloNamespaceOperations">pysharkbite.AccumuloNamespaceOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.AccumuloNamespaceOperations.remove" title="Permalink to this definition">¶</a></dt>
  <dd><p>removes the namespace</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloNamespaceOperations.removeProperty">
  <code class="sig-name descname">removeProperty</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloNamespaceOperations" title="pysharkbite.AccumuloNamespaceOperations">pysharkbite.AccumuloNamespaceOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.AccumuloNamespaceOperations.removeProperty" title="Permalink to this definition">¶</a></dt>
  <dd><p>Remove the namespace property</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloNamespaceOperations.rename">
  <code class="sig-name descname">rename</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloNamespaceOperations" title="pysharkbite.AccumuloNamespaceOperations">pysharkbite.AccumuloNamespaceOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.AccumuloNamespaceOperations.rename" title="Permalink to this definition">¶</a></dt>
  <dd><p>Renames the namespace</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloNamespaceOperations.setProperty">
  <code class="sig-name descname">setProperty</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloNamespaceOperations" title="pysharkbite.AccumuloNamespaceOperations">pysharkbite.AccumuloNamespaceOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg2</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.AccumuloNamespaceOperations.setProperty" title="Permalink to this definition">¶</a></dt>
  <dd><p>Sets a namespace property</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.AccumuloTableOperations">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">AccumuloTableOperations</code><a class="headerlink" href="#pysharkbite.AccumuloTableOperations" title="Permalink to this definition">¶</a></dt>
  <dd><p>Accumulo table operations. Should be accessed through ‘AccumuloConnector’</p>
  <dl class="py method">
  <dt id="pysharkbite.AccumuloTableOperations.addConstraint">
  <code class="sig-name descname">addConstraint</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloTableOperations" title="pysharkbite.AccumuloTableOperations">pysharkbite.AccumuloTableOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.AccumuloTableOperations.addConstraint" title="Permalink to this definition">¶</a></dt>
  <dd><p>Add table constraint</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloTableOperations.addSplits">
  <code class="sig-name descname">addSplits</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloTableOperations" title="pysharkbite.AccumuloTableOperations">pysharkbite.AccumuloTableOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">Set<span class="p">[</span>str<span class="p">]</span></span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.AccumuloTableOperations.addSplits" title="Permalink to this definition">¶</a></dt>
  <dd><p>Add splits for a table</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloTableOperations.compact">
  <code class="sig-name descname">compact</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloTableOperations" title="pysharkbite.AccumuloTableOperations">pysharkbite.AccumuloTableOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg2</span><span class="p">:</span> <span class="n">bool</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.AccumuloTableOperations.compact" title="Permalink to this definition">¶</a></dt>
  <dd><p>compact the table</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloTableOperations.create">
  <code class="sig-name descname">create</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloTableOperations" title="pysharkbite.AccumuloTableOperations">pysharkbite.AccumuloTableOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">bool</span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.AccumuloTableOperations.create" title="Permalink to this definition">¶</a></dt>
  <dd><p>Create the table</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloTableOperations.createScanner">
  <code class="sig-name descname">createScanner</code><span class="sig-paren">(</span><em class="sig-param">self: pysharkbite.AccumuloTableOperations</em>, <em class="sig-param">arg0: cclient::data::security::Authorizations</em>, <em class="sig-param">arg1: int</em><span class="sig-paren">)</span> &#x2192; scanners::Source&lt;cclient::data::KeyValue, scanners::ResultBlock&lt;cclient::data::KeyValue&gt; &gt;<a class="headerlink" href="#pysharkbite.AccumuloTableOperations.createScanner" title="Permalink to this definition">¶</a></dt>
  <dd><p>createWriter scanner</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloTableOperations.createWriter">
  <code class="sig-name descname">createWriter</code><span class="sig-paren">(</span><em class="sig-param">self: pysharkbite.AccumuloTableOperations</em>, <em class="sig-param">arg0: cclient::data::security::Authorizations</em>, <em class="sig-param">arg1: int</em><span class="sig-paren">)</span> &#x2192; writer::Sink&lt;cclient::data::KeyValue&gt;<a class="headerlink" href="#pysharkbite.AccumuloTableOperations.createWriter" title="Permalink to this definition">¶</a></dt>
  <dd><p>Create writer for table</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloTableOperations.exists">
  <code class="sig-name descname">exists</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloTableOperations" title="pysharkbite.AccumuloTableOperations">pysharkbite.AccumuloTableOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">bool</span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.AccumuloTableOperations.exists" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns true if the table exists</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloTableOperations.flush">
  <code class="sig-name descname">flush</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloTableOperations" title="pysharkbite.AccumuloTableOperations">pysharkbite.AccumuloTableOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg2</span><span class="p">:</span> <span class="n">bool</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.AccumuloTableOperations.flush" title="Permalink to this definition">¶</a></dt>
  <dd><p>flush the table</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloTableOperations.import">
  <code class="sig-name descname">import</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloTableOperations" title="pysharkbite.AccumuloTableOperations">pysharkbite.AccumuloTableOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg2</span><span class="p">:</span> <span class="n">bool</span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.AccumuloTableOperations.import" title="Permalink to this definition">¶</a></dt>
  <dd><p>import data into this directory</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloTableOperations.remove">
  <code class="sig-name descname">remove</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloTableOperations" title="pysharkbite.AccumuloTableOperations">pysharkbite.AccumuloTableOperations</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.AccumuloTableOperations.remove" title="Permalink to this definition">¶</a></dt>
  <dd><p>remove the table</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloTableOperations.removeProperty">
  <code class="sig-name descname">removeProperty</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloTableOperations" title="pysharkbite.AccumuloTableOperations">pysharkbite.AccumuloTableOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.AccumuloTableOperations.removeProperty" title="Permalink to this definition">¶</a></dt>
  <dd><p>Remove the table property</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AccumuloTableOperations.setProperty">
  <code class="sig-name descname">setProperty</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloTableOperations" title="pysharkbite.AccumuloTableOperations">pysharkbite.AccumuloTableOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.AccumuloTableOperations.setProperty" title="Permalink to this definition">¶</a></dt>
  <dd><p>Set table property</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.AuthInfo">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">AuthInfo</code><a class="headerlink" href="#pysharkbite.AuthInfo" title="Permalink to this definition">¶</a></dt>
  <dd><p>Auth info object contains accessor to an Accumulo Instance</p>
  <dl class="py method">
  <dt id="pysharkbite.AuthInfo.getInstanceId">
  <code class="sig-name descname">getInstanceId</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AuthInfo" title="pysharkbite.AuthInfo">pysharkbite.AuthInfo</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.AuthInfo.getInstanceId" title="Permalink to this definition">¶</a></dt>
  <dd><p>Get the instance ID</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AuthInfo.getPassword">
  <code class="sig-name descname">getPassword</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AuthInfo" title="pysharkbite.AuthInfo">pysharkbite.AuthInfo</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.AuthInfo.getPassword" title="Permalink to this definition">¶</a></dt>
  <dd><p>Get the user’s password</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.AuthInfo.getUserName">
  <code class="sig-name descname">getUserName</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AuthInfo" title="pysharkbite.AuthInfo">pysharkbite.AuthInfo</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.AuthInfo.getUserName" title="Permalink to this definition">¶</a></dt>
  <dd><p>Get the username</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.Authorizations">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">Authorizations</code><a class="headerlink" href="#pysharkbite.Authorizations" title="Permalink to this definition">¶</a></dt>
  <dd><p>Authorizations object</p>
  <dl class="py method">
  <dt id="pysharkbite.Authorizations.addAuthorization">
  <code class="sig-name descname">addAuthorization</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Authorizations" title="pysharkbite.Authorizations">pysharkbite.Authorizations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.Authorizations.addAuthorization" title="Permalink to this definition">¶</a></dt>
  <dd><p>Add an authorization to be used for table operations</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Authorizations.contains">
  <code class="sig-name descname">contains</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Authorizations" title="pysharkbite.Authorizations">pysharkbite.Authorizations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.Authorizations.contains" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns true if the set contains an authorization</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Authorizations.get_authorizations">
  <code class="sig-name descname">get_authorizations</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Authorizations" title="pysharkbite.Authorizations">pysharkbite.Authorizations</a></span></em><span class="sig-paren">)</span> &#x2192; List<span class="p">[</span>str<span class="p">]</span><a class="headerlink" href="#pysharkbite.Authorizations.get_authorizations" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns an iterable of authorizations</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.BatchScanner">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">BatchScanner</code><a class="headerlink" href="#pysharkbite.BatchScanner" title="Permalink to this definition">¶</a></dt>
  <dd><p>Batch Scanner to be constructed via TableOperations</p>
  <dl class="py method">
  <dt id="pysharkbite.BatchScanner.addIterator">
  <code class="sig-name descname">addIterator</code><span class="sig-paren">(</span><em class="sig-param"><span class="o">*</span><span class="n">args</span></em>, <em class="sig-param"><span class="o">**</span><span class="n">kwargs</span></em><span class="sig-paren">)</span><a class="headerlink" href="#pysharkbite.BatchScanner.addIterator" title="Permalink to this definition">¶</a></dt>
  <dd><p>Overloaded function.</p>
  <ol class="arabic simple">
  <li><p>addIterator(self: pysharkbite.BatchScanner, arg0: pysharkbite.IterInfo) -&gt; None</p></li>
  </ol>
  <p>Add an iterator to be run server side</p>
  <ol class="arabic simple" start="2">
  <li><p>addIterator(self: pysharkbite.BatchScanner, arg0: pysharkbite.PythonIterator) -&gt; None</p></li>
  </ol>
  <p>Adds a python iterator to be run server side</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.BatchScanner.addRange">
  <code class="sig-name descname">addRange</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.BatchScanner" title="pysharkbite.BatchScanner">pysharkbite.BatchScanner</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Range" title="pysharkbite.Range">pysharkbite.Range</a></span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.BatchScanner.addRange" title="Permalink to this definition">¶</a></dt>
  <dd><p>Adds a range</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.BatchScanner.close">
  <code class="sig-name descname">close</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.BatchScanner" title="pysharkbite.BatchScanner">pysharkbite.BatchScanner</a></span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.BatchScanner.close" title="Permalink to this definition">¶</a></dt>
  <dd><p>closes the scanner</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.BatchScanner.fetchColumn">
  <code class="sig-name descname">fetchColumn</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.BatchScanner" title="pysharkbite.BatchScanner">pysharkbite.BatchScanner</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.BatchScanner.fetchColumn" title="Permalink to this definition">¶</a></dt>
  <dd><p>Fetches the column</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.BatchScanner.getResultSet">
  <code class="sig-name descname">getResultSet</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.BatchScanner" title="pysharkbite.BatchScanner">pysharkbite.BatchScanner</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.Results" title="pysharkbite.Results">pysharkbite.Results</a><a class="headerlink" href="#pysharkbite.BatchScanner.getResultSet" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets a result set that can be used asynchronously</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.BatchScanner.removeOption">
  <code class="sig-name descname">removeOption</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.BatchScanner" title="pysharkbite.BatchScanner">pysharkbite.BatchScanner</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.ScannerOptions" title="pysharkbite.ScannerOptions">pysharkbite.ScannerOptions</a></span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.BatchScanner.removeOption" title="Permalink to this definition">¶</a></dt>
  <dd><p>Removes an option</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.BatchScanner.setOption">
  <code class="sig-name descname">setOption</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.BatchScanner" title="pysharkbite.BatchScanner">pysharkbite.BatchScanner</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.ScannerOptions" title="pysharkbite.ScannerOptions">pysharkbite.ScannerOptions</a></span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.BatchScanner.setOption" title="Permalink to this definition">¶</a></dt>
  <dd><p>Sets the option</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.BatchScanner.withRange">
  <code class="sig-name descname">withRange</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.BatchScanner" title="pysharkbite.BatchScanner">pysharkbite.BatchScanner</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Range" title="pysharkbite.Range">pysharkbite.Range</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.BatchScanner" title="pysharkbite.BatchScanner">pysharkbite.BatchScanner</a><a class="headerlink" href="#pysharkbite.BatchScanner.withRange" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.BatchWriter">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">BatchWriter</code><a class="headerlink" href="#pysharkbite.BatchWriter" title="Permalink to this definition">¶</a></dt>
  <dd><p>Batch writer to be constructed, from TableOperations</p>
  <dl class="py method">
  <dt id="pysharkbite.BatchWriter.addMutation">
  <code class="sig-name descname">addMutation</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.BatchWriter" title="pysharkbite.BatchWriter">pysharkbite.BatchWriter</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Mutation" title="pysharkbite.Mutation">pysharkbite.Mutation</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.BatchWriter.addMutation" title="Permalink to this definition">¶</a></dt>
  <dd><p>Adds a mutation to the batch writer</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.BatchWriter.close">
  <code class="sig-name descname">close</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.BatchWriter" title="pysharkbite.BatchWriter">pysharkbite.BatchWriter</a></span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.BatchWriter.close" title="Permalink to this definition">¶</a></dt>
  <dd><p>Closes the batch writer</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.BatchWriter.flush">
  <code class="sig-name descname">flush</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.BatchWriter" title="pysharkbite.BatchWriter">pysharkbite.BatchWriter</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">bool</span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.BatchWriter.flush" title="Permalink to this definition">¶</a></dt>
  <dd><p>Flushes the batch writer. Will be called automatically by close.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.BatchWriter.size">
  <code class="sig-name descname">size</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.BatchWriter" title="pysharkbite.BatchWriter">pysharkbite.BatchWriter</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.BatchWriter.size" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns the current size to be written to Accumulo</p>
  </dd></dl>

  </dd></dl>

  <dl class="py exception">
  <dt id="pysharkbite.ClientException">
  <em class="property">exception </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">ClientException</code><a class="headerlink" href="#pysharkbite.ClientException" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.Compacting">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">Compacting</code><a class="headerlink" href="#pysharkbite.Compacting" title="Permalink to this definition">¶</a></dt>
  <dd><dl class="py method">
  <dt id="pysharkbite.Compacting.getQueued">
  <code class="sig-name descname">getQueued</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Compacting" title="pysharkbite.Compacting">pysharkbite.Compacting</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.Compacting.getQueued" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets queued compactions.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Compacting.getRunning">
  <code class="sig-name descname">getRunning</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Compacting" title="pysharkbite.Compacting">pysharkbite.Compacting</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.Compacting.getRunning" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets running compactions.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Compacting.queued">
  <em class="property">property </em><code class="sig-name descname">queued</code><a class="headerlink" href="#pysharkbite.Compacting.queued" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Compacting.running">
  <em class="property">property </em><code class="sig-name descname">running</code><a class="headerlink" href="#pysharkbite.Compacting.running" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.Configuration">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">Configuration</code><a class="headerlink" href="#pysharkbite.Configuration" title="Permalink to this definition">¶</a></dt>
  <dd><p>Sharkbite Configuration object.</p>
  <dl class="py method">
  <dt id="pysharkbite.Configuration.get">
  <code class="sig-name descname">get</code><span class="sig-paren">(</span><em class="sig-param"><span class="o">*</span><span class="n">args</span></em>, <em class="sig-param"><span class="o">**</span><span class="n">kwargs</span></em><span class="sig-paren">)</span><a class="headerlink" href="#pysharkbite.Configuration.get" title="Permalink to this definition">¶</a></dt>
  <dd><p>Overloaded function.</p>
  <ol class="arabic simple">
  <li><p>get(self: pysharkbite.Configuration, arg0: str) -&gt; str</p></li>
  </ol>
  <p>Get the configuration option</p>
  <ol class="arabic simple" start="2">
  <li><p>get(self: pysharkbite.Configuration, arg0: str, arg1: str) -&gt; str</p></li>
  </ol>
  <p>Get the configuration option with default value</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Configuration.getLong">
  <code class="sig-name descname">getLong</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Configuration" title="pysharkbite.Configuration">pysharkbite.Configuration</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">int</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.Configuration.getLong" title="Permalink to this definition">¶</a></dt>
  <dd><p>Get the integer value of a configuration item</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Configuration.set">
  <code class="sig-name descname">set</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Configuration" title="pysharkbite.Configuration">pysharkbite.Configuration</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.Configuration.set" title="Permalink to this definition">¶</a></dt>
  <dd><p>Set a configuration option</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.CoordinatorGoalState">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">CoordinatorGoalState</code><a class="headerlink" href="#pysharkbite.CoordinatorGoalState" title="Permalink to this definition">¶</a></dt>
  <dd><p>Members:</p>
  <p>CLEAN_STOP : CLEAN_STOP state</p>
  <p>SAFE_MODE : SAFE_MODE state</p>
  <p>NORMAL : NORMAL state</p>
  <dl class="py attribute">
  <dt id="pysharkbite.CoordinatorGoalState.CLEAN_STOP">
  <code class="sig-name descname">CLEAN_STOP</code><em class="property"> = CoordinatorGoalState.CLEAN_STOP</em><a class="headerlink" href="#pysharkbite.CoordinatorGoalState.CLEAN_STOP" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.CoordinatorGoalState.NORMAL">
  <code class="sig-name descname">NORMAL</code><em class="property"> = CoordinatorGoalState.NORMAL</em><a class="headerlink" href="#pysharkbite.CoordinatorGoalState.NORMAL" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.CoordinatorGoalState.SAFE_MODE">
  <code class="sig-name descname">SAFE_MODE</code><em class="property"> = CoordinatorGoalState.SAFE_MODE</em><a class="headerlink" href="#pysharkbite.CoordinatorGoalState.SAFE_MODE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.CoordinatorGoalState.__entries">
  <code class="sig-name descname">__entries</code><em class="property"> = {'CLEAN_STOP': (CoordinatorGoalState.CLEAN_STOP, 'CLEAN_STOP state'), 'NORMAL': (CoordinatorGoalState.NORMAL, 'NORMAL state'), 'SAFE_MODE': (CoordinatorGoalState.SAFE_MODE, 'SAFE_MODE state')}</em><a class="headerlink" href="#pysharkbite.CoordinatorGoalState.__entries" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.CoordinatorGoalState.name">
  <em class="property">property </em><code class="sig-name descname">name</code><a class="headerlink" href="#pysharkbite.CoordinatorGoalState.name" title="Permalink to this definition">¶</a></dt>
  <dd><p>(self: handle) -&gt; str</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.CoordinatorState">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">CoordinatorState</code><a class="headerlink" href="#pysharkbite.CoordinatorState" title="Permalink to this definition">¶</a></dt>
  <dd><p>Members:</p>
  <p>INITIAL : INITIAL state</p>
  <p>HAVE_LOCK : HAVE_LOCK state</p>
  <p>SAFE_MODE : SAFE_MODE state</p>
  <p>NORMAL : NORMAL state</p>
  <p>UNLOAD_METADATA_TABLETS : UNLOAD_METADATA_TABLETS state</p>
  <p>UNLOAD_ROOT_TABLET : UNLOAD_ROOT_TABLET state</p>
  <p>STOP : STOP state</p>
  <dl class="py attribute">
  <dt id="pysharkbite.CoordinatorState.HAVE_LOCK">
  <code class="sig-name descname">HAVE_LOCK</code><em class="property"> = CoordinatorState.HAVE_LOCK</em><a class="headerlink" href="#pysharkbite.CoordinatorState.HAVE_LOCK" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.CoordinatorState.INITIAL">
  <code class="sig-name descname">INITIAL</code><em class="property"> = CoordinatorState.INITIAL</em><a class="headerlink" href="#pysharkbite.CoordinatorState.INITIAL" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.CoordinatorState.NORMAL">
  <code class="sig-name descname">NORMAL</code><em class="property"> = CoordinatorState.NORMAL</em><a class="headerlink" href="#pysharkbite.CoordinatorState.NORMAL" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.CoordinatorState.SAFE_MODE">
  <code class="sig-name descname">SAFE_MODE</code><em class="property"> = CoordinatorState.SAFE_MODE</em><a class="headerlink" href="#pysharkbite.CoordinatorState.SAFE_MODE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.CoordinatorState.STOP">
  <code class="sig-name descname">STOP</code><em class="property"> = CoordinatorState.STOP</em><a class="headerlink" href="#pysharkbite.CoordinatorState.STOP" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.CoordinatorState.UNLOAD_METADATA_TABLETS">
  <code class="sig-name descname">UNLOAD_METADATA_TABLETS</code><em class="property"> = CoordinatorState.UNLOAD_METADATA_TABLETS</em><a class="headerlink" href="#pysharkbite.CoordinatorState.UNLOAD_METADATA_TABLETS" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.CoordinatorState.UNLOAD_ROOT_TABLET">
  <code class="sig-name descname">UNLOAD_ROOT_TABLET</code><em class="property"> = CoordinatorState.UNLOAD_ROOT_TABLET</em><a class="headerlink" href="#pysharkbite.CoordinatorState.UNLOAD_ROOT_TABLET" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.CoordinatorState.__entries">
  <code class="sig-name descname">__entries</code><em class="property"> = {'HAVE_LOCK': (CoordinatorState.HAVE_LOCK, 'HAVE_LOCK state'), 'INITIAL': (CoordinatorState.INITIAL, 'INITIAL state'), 'NORMAL': (CoordinatorState.NORMAL, 'NORMAL state'), 'SAFE_MODE': (CoordinatorState.SAFE_MODE, 'SAFE_MODE state'), 'STOP': (CoordinatorState.STOP, 'STOP state'), 'UNLOAD_METADATA_TABLETS': (CoordinatorState.UNLOAD_METADATA_TABLETS, 'UNLOAD_METADATA_TABLETS state'), 'UNLOAD_ROOT_TABLET': (CoordinatorState.UNLOAD_ROOT_TABLET, 'UNLOAD_ROOT_TABLET state')}</em><a class="headerlink" href="#pysharkbite.CoordinatorState.__entries" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.CoordinatorState.name">
  <em class="property">property </em><code class="sig-name descname">name</code><a class="headerlink" href="#pysharkbite.CoordinatorState.name" title="Permalink to this definition">¶</a></dt>
  <dd><p>(self: handle) -&gt; str</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.DeadServer">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">DeadServer</code><a class="headerlink" href="#pysharkbite.DeadServer" title="Permalink to this definition">¶</a></dt>
  <dd><dl class="py method">
  <dt id="pysharkbite.DeadServer.getLastContact">
  <code class="sig-name descname">getLastContact</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.DeadServer" title="pysharkbite.DeadServer">pysharkbite.DeadServer</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.DeadServer.getLastContact" title="Permalink to this definition">¶</a></dt>
  <dd><p>Get last contact time with this server.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.DeadServer.getServer">
  <code class="sig-name descname">getServer</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.DeadServer" title="pysharkbite.DeadServer">pysharkbite.DeadServer</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.DeadServer.getServer" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the server name.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.DeadServer.getStatus">
  <code class="sig-name descname">getStatus</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.DeadServer" title="pysharkbite.DeadServer">pysharkbite.DeadServer</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.DeadServer.getStatus" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the status of the server.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.DeadServer.last_contact">
  <em class="property">property </em><code class="sig-name descname">last_contact</code><a class="headerlink" href="#pysharkbite.DeadServer.last_contact" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.DeadServer.server">
  <em class="property">property </em><code class="sig-name descname">server</code><a class="headerlink" href="#pysharkbite.DeadServer.server" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.DeadServer.status">
  <em class="property">property </em><code class="sig-name descname">status</code><a class="headerlink" href="#pysharkbite.DeadServer.status" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.Hdfs">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">Hdfs</code><a class="headerlink" href="#pysharkbite.Hdfs" title="Permalink to this definition">¶</a></dt>
  <dd><p>HDFS refernce object</p>
  <dl class="py method">
  <dt id="pysharkbite.Hdfs.chown">
  <code class="sig-name descname">chown</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Hdfs" title="pysharkbite.Hdfs">pysharkbite.Hdfs</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg2</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.Hdfs.chown" title="Permalink to this definition">¶</a></dt>
  <dd><p>Chowns the provided path</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Hdfs.list">
  <code class="sig-name descname">list</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Hdfs" title="pysharkbite.Hdfs">pysharkbite.Hdfs</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; List<span class="p">[</span><a class="reference internal" href="#pysharkbite.HdfsDirEnt" title="pysharkbite.HdfsDirEnt">pysharkbite.HdfsDirEnt</a><span class="p">]</span><a class="headerlink" href="#pysharkbite.Hdfs.list" title="Permalink to this definition">¶</a></dt>
  <dd><p>Lists HDFS directory, returns a list of HdfsDirEnt objects</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Hdfs.mkdir">
  <code class="sig-name descname">mkdir</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Hdfs" title="pysharkbite.Hdfs">pysharkbite.Hdfs</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.Hdfs.mkdir" title="Permalink to this definition">¶</a></dt>
  <dd><p>Creates a directory on HDFS. Should be a relative path</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Hdfs.move">
  <code class="sig-name descname">move</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Hdfs" title="pysharkbite.Hdfs">pysharkbite.Hdfs</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.Hdfs.move" title="Permalink to this definition">¶</a></dt>
  <dd><p>Moves a file within a NN instance.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Hdfs.read">
  <code class="sig-name descname">read</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Hdfs" title="pysharkbite.Hdfs">pysharkbite.Hdfs</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; cclient::data::streams::InputStream<a class="headerlink" href="#pysharkbite.Hdfs.read" title="Permalink to this definition">¶</a></dt>
  <dd><p>Opens a read stream to an HDFS file, creating or updating it</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Hdfs.remove">
  <code class="sig-name descname">remove</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Hdfs" title="pysharkbite.Hdfs">pysharkbite.Hdfs</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">bool</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.Hdfs.remove" title="Permalink to this definition">¶</a></dt>
  <dd><p>Removes a file or directory, the boolean flag, if set to true, deletes recusively</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Hdfs.rename">
  <code class="sig-name descname">rename</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Hdfs" title="pysharkbite.Hdfs">pysharkbite.Hdfs</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.Hdfs.rename" title="Permalink to this definition">¶</a></dt>
  <dd><p>Renames a path</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Hdfs.write">
  <code class="sig-name descname">write</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Hdfs" title="pysharkbite.Hdfs">pysharkbite.Hdfs</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; cclient::data::streams::ByteOutputStream<a class="headerlink" href="#pysharkbite.Hdfs.write" title="Permalink to this definition">¶</a></dt>
  <dd><p>Opens a write stream to an HDFS file, creating or updating it</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.HdfsDirEnt">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">HdfsDirEnt</code><a class="headerlink" href="#pysharkbite.HdfsDirEnt" title="Permalink to this definition">¶</a></dt>
  <dd><p>HDFS directory entry object.</p>
  <dl class="py method">
  <dt id="pysharkbite.HdfsDirEnt.getGroup">
  <code class="sig-name descname">getGroup</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.HdfsDirEnt" title="pysharkbite.HdfsDirEnt">pysharkbite.HdfsDirEnt</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.HdfsDirEnt.getGroup" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the group of the directory entry</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.HdfsDirEnt.getName">
  <code class="sig-name descname">getName</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.HdfsDirEnt" title="pysharkbite.HdfsDirEnt">pysharkbite.HdfsDirEnt</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.HdfsDirEnt.getName" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the name of the directory entry</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.HdfsDirEnt.getOwner">
  <code class="sig-name descname">getOwner</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.HdfsDirEnt" title="pysharkbite.HdfsDirEnt">pysharkbite.HdfsDirEnt</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.HdfsDirEnt.getOwner" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the owner of the directory entry</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.HdfsDirEnt.getSize">
  <code class="sig-name descname">getSize</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.HdfsDirEnt" title="pysharkbite.HdfsDirEnt">pysharkbite.HdfsDirEnt</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.HdfsDirEnt.getSize" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns the size of the directory entry</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.HdfsInputStream">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">HdfsInputStream</code><a class="headerlink" href="#pysharkbite.HdfsInputStream" title="Permalink to this definition">¶</a></dt>
  <dd><p>HDFS Input Stream</p>
  <dl class="py method">
  <dt id="pysharkbite.HdfsInputStream.readBytes">
  <code class="sig-name descname">readBytes</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.HdfsInputStream" title="pysharkbite.HdfsInputStream">pysharkbite.HdfsInputStream</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">int</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.HdfsInputStream.readBytes" title="Permalink to this definition">¶</a></dt>
  <dd><p>Reads a character sequence from the file on HDFS</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.HdfsInputStream.readInt">
  <code class="sig-name descname">readInt</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.HdfsInputStream" title="pysharkbite.HdfsInputStream">pysharkbite.HdfsInputStream</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.HdfsInputStream.readInt" title="Permalink to this definition">¶</a></dt>
  <dd><p>Reads a four byte integer</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.HdfsInputStream.readLong">
  <code class="sig-name descname">readLong</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.HdfsInputStream" title="pysharkbite.HdfsInputStream">pysharkbite.HdfsInputStream</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.HdfsInputStream.readLong" title="Permalink to this definition">¶</a></dt>
  <dd><p>Reads an eight byte integer</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.HdfsInputStream.readShort">
  <code class="sig-name descname">readShort</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.HdfsInputStream" title="pysharkbite.HdfsInputStream">pysharkbite.HdfsInputStream</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.HdfsInputStream.readShort" title="Permalink to this definition">¶</a></dt>
  <dd><p>Reads a two byte integer</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.HdfsInputStream.readString">
  <code class="sig-name descname">readString</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.HdfsInputStream" title="pysharkbite.HdfsInputStream">pysharkbite.HdfsInputStream</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.HdfsInputStream.readString" title="Permalink to this definition">¶</a></dt>
  <dd><p>Reads a string</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.HdfsOutputStream">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">HdfsOutputStream</code><a class="headerlink" href="#pysharkbite.HdfsOutputStream" title="Permalink to this definition">¶</a></dt>
  <dd><p>HDFS Output Stream</p>
  <dl class="py method">
  <dt id="pysharkbite.HdfsOutputStream.write">
  <code class="sig-name descname">write</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.HdfsOutputStream" title="pysharkbite.HdfsOutputStream">pysharkbite.HdfsOutputStream</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">int</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.HdfsOutputStream.write" title="Permalink to this definition">¶</a></dt>
  <dd><p>Writes an eight byte integer</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.HdfsOutputStream.writeInt">
  <code class="sig-name descname">writeInt</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.HdfsOutputStream" title="pysharkbite.HdfsOutputStream">pysharkbite.HdfsOutputStream</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">int</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.HdfsOutputStream.writeInt" title="Permalink to this definition">¶</a></dt>
  <dd><p>Writes a four byte integer</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.HdfsOutputStream.writeLong">
  <code class="sig-name descname">writeLong</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.HdfsOutputStream" title="pysharkbite.HdfsOutputStream">pysharkbite.HdfsOutputStream</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">int</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.HdfsOutputStream.writeLong" title="Permalink to this definition">¶</a></dt>
  <dd><p>Writes an eight byte integer</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.HdfsOutputStream.writeShort">
  <code class="sig-name descname">writeShort</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.HdfsOutputStream" title="pysharkbite.HdfsOutputStream">pysharkbite.HdfsOutputStream</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">int</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.HdfsOutputStream.writeShort" title="Permalink to this definition">¶</a></dt>
  <dd><p>Writes a two byte integer</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.HdfsOutputStream.writeString">
  <code class="sig-name descname">writeString</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.HdfsOutputStream" title="pysharkbite.HdfsOutputStream">pysharkbite.HdfsOutputStream</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.HdfsOutputStream.writeString" title="Permalink to this definition">¶</a></dt>
  <dd><p>Writes a string</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.Instance">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">Instance</code><a class="headerlink" href="#pysharkbite.Instance" title="Permalink to this definition">¶</a></dt>
  <dd><p>Accumulo Instance</p>
  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.IterInfo">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">IterInfo</code><a class="headerlink" href="#pysharkbite.IterInfo" title="Permalink to this definition">¶</a></dt>
  <dd><p>IterInfo is an iterator configuration</p>
  <dl class="py method">
  <dt id="pysharkbite.IterInfo.getClass">
  <code class="sig-name descname">getClass</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.IterInfo" title="pysharkbite.IterInfo">pysharkbite.IterInfo</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.IterInfo.getClass" title="Permalink to this definition">¶</a></dt>
  <dd><p>Get the class for this iterator</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.IterInfo.getName">
  <code class="sig-name descname">getName</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.IterInfo" title="pysharkbite.IterInfo">pysharkbite.IterInfo</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.IterInfo.getName" title="Permalink to this definition">¶</a></dt>
  <dd><p>Get the name of this iterator</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.IterInfo.getPriority">
  <code class="sig-name descname">getPriority</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.IterInfo" title="pysharkbite.IterInfo">pysharkbite.IterInfo</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.IterInfo.getPriority" title="Permalink to this definition">¶</a></dt>
  <dd><p>Get the priority for this iterator</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.Key">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">Key</code><a class="headerlink" href="#pysharkbite.Key" title="Permalink to this definition">¶</a></dt>
  <dd><p>Accumulo Key</p>
  <dl class="py method">
  <dt id="pysharkbite.Key.getColumnFamily">
  <code class="sig-name descname">getColumnFamily</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Key" title="pysharkbite.Key">pysharkbite.Key</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.Key.getColumnFamily" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the Column Family</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Key.getColumnQualifier">
  <code class="sig-name descname">getColumnQualifier</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Key" title="pysharkbite.Key">pysharkbite.Key</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.Key.getColumnQualifier" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the Column Qualifier</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Key.getColumnVisibility">
  <code class="sig-name descname">getColumnVisibility</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Key" title="pysharkbite.Key">pysharkbite.Key</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.Key.getColumnVisibility" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the Column Visibility</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Key.getRow">
  <code class="sig-name descname">getRow</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Key" title="pysharkbite.Key">pysharkbite.Key</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.Key.getRow" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the row</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Key.getTimestamp">
  <code class="sig-name descname">getTimestamp</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Key" title="pysharkbite.Key">pysharkbite.Key</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.Key.getTimestamp" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the key timestamp</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Key.setColumnFamily">
  <code class="sig-name descname">setColumnFamily</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Key" title="pysharkbite.Key">pysharkbite.Key</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.Key.setColumnFamily" title="Permalink to this definition">¶</a></dt>
  <dd><p>Sets the column fmaily</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Key.setColumnQualifier">
  <code class="sig-name descname">setColumnQualifier</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Key" title="pysharkbite.Key">pysharkbite.Key</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.Key.setColumnQualifier" title="Permalink to this definition">¶</a></dt>
  <dd><p>Sets the column qualifier</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Key.setRow">
  <code class="sig-name descname">setRow</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Key" title="pysharkbite.Key">pysharkbite.Key</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.Key.setRow" title="Permalink to this definition">¶</a></dt>
  <dd><p>Sets the row</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.KeyValue">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">KeyValue</code><a class="headerlink" href="#pysharkbite.KeyValue" title="Permalink to this definition">¶</a></dt>
  <dd><dl class="py method">
  <dt id="pysharkbite.KeyValue.getKey">
  <code class="sig-name descname">getKey</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.KeyValue" title="pysharkbite.KeyValue">pysharkbite.KeyValue</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.Key" title="pysharkbite.Key">pysharkbite.Key</a><a class="headerlink" href="#pysharkbite.KeyValue.getKey" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the Key from the key value object</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.KeyValue.getValue">
  <code class="sig-name descname">getValue</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.KeyValue" title="pysharkbite.KeyValue">pysharkbite.KeyValue</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.Value" title="pysharkbite.Value">pysharkbite.Value</a><a class="headerlink" href="#pysharkbite.KeyValue.getValue" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the Value from the key value object</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.KeyValueIterator">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">KeyValueIterator</code><a class="headerlink" href="#pysharkbite.KeyValueIterator" title="Permalink to this definition">¶</a></dt>
  <dd><p>Iterable object within Accumulo</p>
  <dl class="py method">
  <dt id="pysharkbite.KeyValueIterator.getTopKey">
  <code class="sig-name descname">getTopKey</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.KeyValueIterator" title="pysharkbite.KeyValueIterator">pysharkbite.KeyValueIterator</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.Key" title="pysharkbite.Key">pysharkbite.Key</a><a class="headerlink" href="#pysharkbite.KeyValueIterator.getTopKey" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns the top key</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.KeyValueIterator.getTopValue">
  <code class="sig-name descname">getTopValue</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.KeyValueIterator" title="pysharkbite.KeyValueIterator">pysharkbite.KeyValueIterator</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.Value" title="pysharkbite.Value">pysharkbite.Value</a><a class="headerlink" href="#pysharkbite.KeyValueIterator.getTopValue" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns the top value</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.KeyValueIterator.hasNext">
  <code class="sig-name descname">hasNext</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.KeyValueIterator" title="pysharkbite.KeyValueIterator">pysharkbite.KeyValueIterator</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.KeyValueIterator.hasNext" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns true of further keys exist, false otherwise</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.KeyValueIterator.next">
  <code class="sig-name descname">next</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.KeyValueIterator" title="pysharkbite.KeyValueIterator">pysharkbite.KeyValueIterator</a></span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.KeyValueIterator.next" title="Permalink to this definition">¶</a></dt>
  <dd><p>Queues the next key/value to be returned via getTopKey and getTopValue</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.KeyValueIterator.seek">
  <code class="sig-name descname">seek</code><span class="sig-paren">(</span><em class="sig-param">self: pysharkbite.KeyValueIterator</em>, <em class="sig-param">arg0: cclient::data::streams::StreamRelocation</em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.KeyValueIterator.seek" title="Permalink to this definition">¶</a></dt>
  <dd><p>Seeks to a key within the iterator</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.LoggingConfiguration">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">LoggingConfiguration</code><a class="headerlink" href="#pysharkbite.LoggingConfiguration" title="Permalink to this definition">¶</a></dt>
  <dd><p>Sharkbite Logger, enables logging at the debug and trace levels</p>
  <dl class="py method">
  <dt id="pysharkbite.LoggingConfiguration.enableDebugLogger">
  <em class="property">static </em><code class="sig-name descname">enableDebugLogger</code><span class="sig-paren">(</span><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.LoggingConfiguration.enableDebugLogger" title="Permalink to this definition">¶</a></dt>
  <dd><p>Enables the debug logging for all classes</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.LoggingConfiguration.enableTraceLogger">
  <em class="property">static </em><code class="sig-name descname">enableTraceLogger</code><span class="sig-paren">(</span><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.LoggingConfiguration.enableTraceLogger" title="Permalink to this definition">¶</a></dt>
  <dd><p>Enables trace logging for all classes</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.Mutation">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">Mutation</code><a class="headerlink" href="#pysharkbite.Mutation" title="Permalink to this definition">¶</a></dt>
  <dd><dl class="py method">
  <dt id="pysharkbite.Mutation.put">
  <code class="sig-name descname">put</code><span class="sig-paren">(</span><em class="sig-param"><span class="o">*</span><span class="n">args</span></em>, <em class="sig-param"><span class="o">**</span><span class="n">kwargs</span></em><span class="sig-paren">)</span><a class="headerlink" href="#pysharkbite.Mutation.put" title="Permalink to this definition">¶</a></dt>
  <dd><p>Overloaded function.</p>
  <ol class="arabic simple">
  <li><p>put(self: pysharkbite.Mutation, arg0: str, arg1: str, arg2: str, arg3: int, arg4: str) -&gt; None</p></li>
  </ol>
  <p>Adds a mutation</p>
  <ol class="arabic simple" start="2">
  <li><p>put(self: pysharkbite.Mutation, arg0: str, arg1: str, arg2: str, arg3: int) -&gt; None</p></li>
  </ol>
  <p>Adds a mutation</p>
  <ol class="arabic simple" start="3">
  <li><p>put(self: pysharkbite.Mutation, arg0: str, arg1: str, arg2: str) -&gt; None</p></li>
  </ol>
  <p>Adds a mutation</p>
  <ol class="arabic simple" start="4">
  <li><p>put(self: pysharkbite.Mutation, arg0: str, arg1: str) -&gt; None</p></li>
  </ol>
  <p>Adds a mutation</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Mutation.putDelete">
  <code class="sig-name descname">putDelete</code><span class="sig-paren">(</span><em class="sig-param"><span class="o">*</span><span class="n">args</span></em>, <em class="sig-param"><span class="o">**</span><span class="n">kwargs</span></em><span class="sig-paren">)</span><a class="headerlink" href="#pysharkbite.Mutation.putDelete" title="Permalink to this definition">¶</a></dt>
  <dd><p>Overloaded function.</p>
  <ol class="arabic simple">
  <li><p>putDelete(self: pysharkbite.Mutation, arg0: str, arg1: str, arg2: str, arg3: int) -&gt; None</p></li>
  </ol>
  <p>Adds a delete mutation</p>
  <ol class="arabic simple" start="2">
  <li><p>putDelete(self: pysharkbite.Mutation, arg0: str, arg1: str, arg2: str) -&gt; None</p></li>
  </ol>
  <p>Adds a delete mutation</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.NamespacePermissions">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">NamespacePermissions</code><a class="headerlink" href="#pysharkbite.NamespacePermissions" title="Permalink to this definition">¶</a></dt>
  <dd><p>Members:</p>
  <p>READ : Enables read permission on the namespace</p>
  <p>WRITE : Enables write permission on the namespace</p>
  <p>ALTER_NAMESPACE : Enables the alter namespace permissions for the user</p>
  <p>GRANT : Enables grant permission on the namespace</p>
  <p>ALTER_TABLE : Enables alter table permissions on the namespace</p>
  <p>CREATE_TABLE : Enables create table permissions on the namespace</p>
  <p>DROP_TABLE : Enables drop table permissions on the namespace</p>
  <p>BULK_IMPORT : Enables bulk import permissions on the namespace</p>
  <p>DROP_NAMESPACE : Enables drop namespace permissions on the namespace</p>
  <dl class="py attribute">
  <dt id="pysharkbite.NamespacePermissions.ALTER_NAMESPACE">
  <code class="sig-name descname">ALTER_NAMESPACE</code><em class="property"> = NamespacePermissions.ALTER_NAMESPACE</em><a class="headerlink" href="#pysharkbite.NamespacePermissions.ALTER_NAMESPACE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.NamespacePermissions.ALTER_TABLE">
  <code class="sig-name descname">ALTER_TABLE</code><em class="property"> = NamespacePermissions.ALTER_TABLE</em><a class="headerlink" href="#pysharkbite.NamespacePermissions.ALTER_TABLE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.NamespacePermissions.BULK_IMPORT">
  <code class="sig-name descname">BULK_IMPORT</code><em class="property"> = NamespacePermissions.BULK_IMPORT</em><a class="headerlink" href="#pysharkbite.NamespacePermissions.BULK_IMPORT" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.NamespacePermissions.CREATE_TABLE">
  <code class="sig-name descname">CREATE_TABLE</code><em class="property"> = NamespacePermissions.CREATE_TABLE</em><a class="headerlink" href="#pysharkbite.NamespacePermissions.CREATE_TABLE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.NamespacePermissions.DROP_NAMESPACE">
  <code class="sig-name descname">DROP_NAMESPACE</code><em class="property"> = NamespacePermissions.DROP_NAMESPACE</em><a class="headerlink" href="#pysharkbite.NamespacePermissions.DROP_NAMESPACE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.NamespacePermissions.DROP_TABLE">
  <code class="sig-name descname">DROP_TABLE</code><em class="property"> = NamespacePermissions.DROP_TABLE</em><a class="headerlink" href="#pysharkbite.NamespacePermissions.DROP_TABLE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.NamespacePermissions.GRANT">
  <code class="sig-name descname">GRANT</code><em class="property"> = NamespacePermissions.GRANT</em><a class="headerlink" href="#pysharkbite.NamespacePermissions.GRANT" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.NamespacePermissions.READ">
  <code class="sig-name descname">READ</code><em class="property"> = NamespacePermissions.READ</em><a class="headerlink" href="#pysharkbite.NamespacePermissions.READ" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.NamespacePermissions.WRITE">
  <code class="sig-name descname">WRITE</code><em class="property"> = NamespacePermissions.WRITE</em><a class="headerlink" href="#pysharkbite.NamespacePermissions.WRITE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.NamespacePermissions.__entries">
  <code class="sig-name descname">__entries</code><em class="property"> = {'ALTER_NAMESPACE': (NamespacePermissions.ALTER_NAMESPACE, 'Enables the alter namespace permissions for the user'), 'ALTER_TABLE': (NamespacePermissions.ALTER_TABLE, 'Enables alter table permissions on the namespace'), 'BULK_IMPORT': (NamespacePermissions.BULK_IMPORT, 'Enables bulk import permissions on the namespace'), 'CREATE_TABLE': (NamespacePermissions.CREATE_TABLE, 'Enables create table permissions on the namespace'), 'DROP_NAMESPACE': (NamespacePermissions.DROP_NAMESPACE, 'Enables drop namespace permissions on the namespace'), 'DROP_TABLE': (NamespacePermissions.DROP_TABLE, 'Enables drop table permissions on the namespace'), 'GRANT': (NamespacePermissions.GRANT, 'Enables grant permission on the namespace'), 'READ': (NamespacePermissions.READ, 'Enables read permission on the namespace'), 'WRITE': (NamespacePermissions.WRITE, 'Enables write permission on the namespace')}</em><a class="headerlink" href="#pysharkbite.NamespacePermissions.__entries" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.NamespacePermissions.name">
  <em class="property">property </em><code class="sig-name descname">name</code><a class="headerlink" href="#pysharkbite.NamespacePermissions.name" title="Permalink to this definition">¶</a></dt>
  <dd><p>(self: handle) -&gt; str</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.PythonIterator">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">PythonIterator</code><a class="headerlink" href="#pysharkbite.PythonIterator" title="Permalink to this definition">¶</a></dt>
  <dd><p>Defines a python iterator</p>
  <dl class="py method">
  <dt id="pysharkbite.PythonIterator.getClass">
  <code class="sig-name descname">getClass</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.PythonIterator" title="pysharkbite.PythonIterator">pysharkbite.PythonIterator</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.PythonIterator.getClass" title="Permalink to this definition">¶</a></dt>
  <dd><p>Get the class for this iterator</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.PythonIterator.getName">
  <code class="sig-name descname">getName</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.PythonIterator" title="pysharkbite.PythonIterator">pysharkbite.PythonIterator</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.PythonIterator.getName" title="Permalink to this definition">¶</a></dt>
  <dd><p>Get the name of this iterator</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.PythonIterator.getPriority">
  <code class="sig-name descname">getPriority</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.PythonIterator" title="pysharkbite.PythonIterator">pysharkbite.PythonIterator</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.PythonIterator.getPriority" title="Permalink to this definition">¶</a></dt>
  <dd><p>Get the priority for this iterator</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.PythonIterator.onNext">
  <code class="sig-name descname">onNext</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.PythonIterator" title="pysharkbite.PythonIterator">pysharkbite.PythonIterator</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.PythonIterator" title="pysharkbite.PythonIterator">pysharkbite.PythonIterator</a><a class="headerlink" href="#pysharkbite.PythonIterator.onNext" title="Permalink to this definition">¶</a></dt>
  <dd><p>Lambda that is provided the accumulo key</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.RFile">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">RFile</code><a class="headerlink" href="#pysharkbite.RFile" title="Permalink to this definition">¶</a></dt>
  <dd><p>RFile, which is an internal data structure for storing data within Accumulo</p>
  <dl class="py method">
  <dt id="pysharkbite.RFile.close">
  <code class="sig-name descname">close</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.RFile" title="pysharkbite.RFile">pysharkbite.RFile</a></span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.RFile.close" title="Permalink to this definition">¶</a></dt>
  <dd><p>Closes the RFile</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.RFile.getTop">
  <code class="sig-name descname">getTop</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.RFile" title="pysharkbite.RFile">pysharkbite.RFile</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.KeyValue" title="pysharkbite.KeyValue">pysharkbite.KeyValue</a><a class="headerlink" href="#pysharkbite.RFile.getTop" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns the top key/value</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.RFile.hasNext">
  <code class="sig-name descname">hasNext</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.RFile" title="pysharkbite.RFile">pysharkbite.RFile</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.RFile.hasNext" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns true of further keys exist, false otherwise</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.RFile.next">
  <code class="sig-name descname">next</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.RFile" title="pysharkbite.RFile">pysharkbite.RFile</a></span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.RFile.next" title="Permalink to this definition">¶</a></dt>
  <dd><p>Queues the next key/value pair</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.RFile.seek">
  <code class="sig-name descname">seek</code><span class="sig-paren">(</span><em class="sig-param">self: pysharkbite.RFile</em>, <em class="sig-param">arg0: cclient::data::streams::StreamRelocation</em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.RFile.seek" title="Permalink to this definition">¶</a></dt>
  <dd><p>Seek to data</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.RFileOperations">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">RFileOperations</code><a class="headerlink" href="#pysharkbite.RFileOperations" title="Permalink to this definition">¶</a></dt>
  <dd><p>RFile Operations</p>
  <dl class="py method">
  <dt id="pysharkbite.RFileOperations.openForWrite">
  <code class="sig-name descname">openForWrite</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.SequentialRFile" title="pysharkbite.SequentialRFile">pysharkbite.SequentialRFile</a><a class="headerlink" href="#pysharkbite.RFileOperations.openForWrite" title="Permalink to this definition">¶</a></dt>
  <dd><p>Opens an RFile to write</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.RFileOperations.openManySequential">
  <code class="sig-name descname">openManySequential</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n">List<span class="p">[</span>str<span class="p">]</span></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">int</span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.KeyValueIterator" title="pysharkbite.KeyValueIterator">pysharkbite.KeyValueIterator</a><a class="headerlink" href="#pysharkbite.RFileOperations.openManySequential" title="Permalink to this definition">¶</a></dt>
  <dd><p>Opens many RFiles sequentially.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.RFileOperations.randomSeek">
  <code class="sig-name descname">randomSeek</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.RFile" title="pysharkbite.RFile">pysharkbite.RFile</a><a class="headerlink" href="#pysharkbite.RFileOperations.randomSeek" title="Permalink to this definition">¶</a></dt>
  <dd><p>Opens a single RFile to read, best used for random seeks</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.RFileOperations.sequentialRead">
  <code class="sig-name descname">sequentialRead</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.SequentialRFile" title="pysharkbite.SequentialRFile">pysharkbite.SequentialRFile</a><a class="headerlink" href="#pysharkbite.RFileOperations.sequentialRead" title="Permalink to this definition">¶</a></dt>
  <dd><p>Opens an RFile to read sequentially</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.Range">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">Range</code><a class="headerlink" href="#pysharkbite.Range" title="Permalink to this definition">¶</a></dt>
  <dd><dl class="py method">
  <dt id="pysharkbite.Range.after_end_key">
  <code class="sig-name descname">after_end_key</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Range" title="pysharkbite.Range">pysharkbite.Range</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Key" title="pysharkbite.Key">pysharkbite.Key</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.Range.after_end_key" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns true if the provided key is after this range’s end key</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Range.before_start_key">
  <code class="sig-name descname">before_start_key</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Range" title="pysharkbite.Range">pysharkbite.Range</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Key" title="pysharkbite.Key">pysharkbite.Key</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.Range.before_start_key" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns true if the provided key is before this range’s start key</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Range.get_start_key">
  <code class="sig-name descname">get_start_key</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Range" title="pysharkbite.Range">pysharkbite.Range</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.Key" title="pysharkbite.Key">pysharkbite.Key</a><a class="headerlink" href="#pysharkbite.Range.get_start_key" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns the start key of this range, if it exists</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Range.get_stop_key">
  <code class="sig-name descname">get_stop_key</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Range" title="pysharkbite.Range">pysharkbite.Range</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.Key" title="pysharkbite.Key">pysharkbite.Key</a><a class="headerlink" href="#pysharkbite.Range.get_stop_key" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns the stop key of this range, if it exists</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Range.inifinite_start_key">
  <code class="sig-name descname">inifinite_start_key</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Range" title="pysharkbite.Range">pysharkbite.Range</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.Range.inifinite_start_key" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns true if the start key is inclusive</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Range.inifinite_stop_key">
  <code class="sig-name descname">inifinite_stop_key</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Range" title="pysharkbite.Range">pysharkbite.Range</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.Range.inifinite_stop_key" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns true if the stop key is inclusive</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Range.start_key_inclusive">
  <code class="sig-name descname">start_key_inclusive</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Range" title="pysharkbite.Range">pysharkbite.Range</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.Range.start_key_inclusive" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns true if the start key is inclusive</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Range.stop_key_inclusive">
  <code class="sig-name descname">stop_key_inclusive</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Range" title="pysharkbite.Range">pysharkbite.Range</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.Range.stop_key_inclusive" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns true if the stop key is inclusive</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.RecoveryStatus">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">RecoveryStatus</code><a class="headerlink" href="#pysharkbite.RecoveryStatus" title="Permalink to this definition">¶</a></dt>
  <dd><dl class="py method">
  <dt id="pysharkbite.RecoveryStatus.getName">
  <code class="sig-name descname">getName</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.RecoveryStatus" title="pysharkbite.RecoveryStatus">pysharkbite.RecoveryStatus</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.RecoveryStatus.getName" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the name of the recovery.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.RecoveryStatus.getProgress">
  <code class="sig-name descname">getProgress</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.RecoveryStatus" title="pysharkbite.RecoveryStatus">pysharkbite.RecoveryStatus</a></span></em><span class="sig-paren">)</span> &#x2192; float<a class="headerlink" href="#pysharkbite.RecoveryStatus.getProgress" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the progress of the recovery.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.RecoveryStatus.getRuntime">
  <code class="sig-name descname">getRuntime</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.RecoveryStatus" title="pysharkbite.RecoveryStatus">pysharkbite.RecoveryStatus</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.RecoveryStatus.getRuntime" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the runtime of the recovery process.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.RecoveryStatus.name">
  <em class="property">property </em><code class="sig-name descname">name</code><a class="headerlink" href="#pysharkbite.RecoveryStatus.name" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.RecoveryStatus.progress">
  <em class="property">property </em><code class="sig-name descname">progress</code><a class="headerlink" href="#pysharkbite.RecoveryStatus.progress" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.RecoveryStatus.runtime">
  <em class="property">property </em><code class="sig-name descname">runtime</code><a class="headerlink" href="#pysharkbite.RecoveryStatus.runtime" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.Results">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">Results</code><a class="headerlink" href="#pysharkbite.Results" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.ScannerOptions">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">ScannerOptions</code><a class="headerlink" href="#pysharkbite.ScannerOptions" title="Permalink to this definition">¶</a></dt>
  <dd><p>Members:</p>
  <p>HedgedReads : Enables hedged reads</p>
  <p>RFileScanOnly : Enables the RFile Scanner</p>
  <dl class="py attribute">
  <dt id="pysharkbite.ScannerOptions.HedgedReads">
  <code class="sig-name descname">HedgedReads</code><em class="property"> = ScannerOptions.HedgedReads</em><a class="headerlink" href="#pysharkbite.ScannerOptions.HedgedReads" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.ScannerOptions.RFileScanOnly">
  <code class="sig-name descname">RFileScanOnly</code><em class="property"> = ScannerOptions.RFileScanOnly</em><a class="headerlink" href="#pysharkbite.ScannerOptions.RFileScanOnly" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.ScannerOptions.__entries">
  <code class="sig-name descname">__entries</code><em class="property"> = {'HedgedReads': (ScannerOptions.HedgedReads, 'Enables hedged reads'), 'RFileScanOnly': (ScannerOptions.RFileScanOnly, 'Enables the RFile Scanner')}</em><a class="headerlink" href="#pysharkbite.ScannerOptions.__entries" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.ScannerOptions.name">
  <em class="property">property </em><code class="sig-name descname">name</code><a class="headerlink" href="#pysharkbite.ScannerOptions.name" title="Permalink to this definition">¶</a></dt>
  <dd><p>(self: handle) -&gt; str</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.SecurityOperations">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">SecurityOperations</code><a class="headerlink" href="#pysharkbite.SecurityOperations" title="Permalink to this definition">¶</a></dt>
  <dd><p>Basic security operations</p>
  <dl class="py method">
  <dt id="pysharkbite.SecurityOperations.change_password">
  <code class="sig-name descname">change_password</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SecurityOperations" title="pysharkbite.SecurityOperations">pysharkbite.SecurityOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.SecurityOperations.change_password" title="Permalink to this definition">¶</a></dt>
  <dd><p>Changes the user password</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SecurityOperations.create_user">
  <code class="sig-name descname">create_user</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SecurityOperations" title="pysharkbite.SecurityOperations">pysharkbite.SecurityOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.SecurityOperations.create_user" title="Permalink to this definition">¶</a></dt>
  <dd><p>Creates a user</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SecurityOperations.get_auths">
  <code class="sig-name descname">get_auths</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SecurityOperations" title="pysharkbite.SecurityOperations">pysharkbite.SecurityOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.Authorizations" title="pysharkbite.Authorizations">pysharkbite.Authorizations</a><a class="headerlink" href="#pysharkbite.SecurityOperations.get_auths" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns the user’s authorizations</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SecurityOperations.grantAuthorizations">
  <code class="sig-name descname">grantAuthorizations</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SecurityOperations" title="pysharkbite.SecurityOperations">pysharkbite.SecurityOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Authorizations" title="pysharkbite.Authorizations">pysharkbite.Authorizations</a></span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.SecurityOperations.grantAuthorizations" title="Permalink to this definition">¶</a></dt>
  <dd><p>Get user authorizations</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SecurityOperations.grant_namespace_permission">
  <code class="sig-name descname">grant_namespace_permission</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SecurityOperations" title="pysharkbite.SecurityOperations">pysharkbite.SecurityOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg2</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.NamespacePermissions" title="pysharkbite.NamespacePermissions">pysharkbite.NamespacePermissions</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.SecurityOperations.grant_namespace_permission" title="Permalink to this definition">¶</a></dt>
  <dd><p>Grants a namespace permission</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SecurityOperations.grant_system_permission">
  <code class="sig-name descname">grant_system_permission</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SecurityOperations" title="pysharkbite.SecurityOperations">pysharkbite.SecurityOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SystemPermissions" title="pysharkbite.SystemPermissions">pysharkbite.SystemPermissions</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.SecurityOperations.grant_system_permission" title="Permalink to this definition">¶</a></dt>
  <dd><p>Grants a system permission</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SecurityOperations.grant_table_permission">
  <code class="sig-name descname">grant_table_permission</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SecurityOperations" title="pysharkbite.SecurityOperations">pysharkbite.SecurityOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg2</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TablePermissions" title="pysharkbite.TablePermissions">pysharkbite.TablePermissions</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.SecurityOperations.grant_table_permission" title="Permalink to this definition">¶</a></dt>
  <dd><p>Grants a table permission</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SecurityOperations.has_namespace_permission">
  <code class="sig-name descname">has_namespace_permission</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SecurityOperations" title="pysharkbite.SecurityOperations">pysharkbite.SecurityOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg2</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.NamespacePermissions" title="pysharkbite.NamespacePermissions">pysharkbite.NamespacePermissions</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.SecurityOperations.has_namespace_permission" title="Permalink to this definition">¶</a></dt>
  <dd><p>Has namespace permission</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SecurityOperations.has_system_permission">
  <code class="sig-name descname">has_system_permission</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SecurityOperations" title="pysharkbite.SecurityOperations">pysharkbite.SecurityOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SystemPermissions" title="pysharkbite.SystemPermissions">pysharkbite.SystemPermissions</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.SecurityOperations.has_system_permission" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns true if the user has the system permission</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SecurityOperations.has_table_permission">
  <code class="sig-name descname">has_table_permission</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SecurityOperations" title="pysharkbite.SecurityOperations">pysharkbite.SecurityOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg2</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TablePermissions" title="pysharkbite.TablePermissions">pysharkbite.TablePermissions</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.SecurityOperations.has_table_permission" title="Permalink to this definition">¶</a></dt>
  <dd><p>Has table permission</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SecurityOperations.remove_user">
  <code class="sig-name descname">remove_user</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SecurityOperations" title="pysharkbite.SecurityOperations">pysharkbite.SecurityOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.SecurityOperations.remove_user" title="Permalink to this definition">¶</a></dt>
  <dd><p>Removes the user</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SecurityOperations.revoke_namespace_permission">
  <code class="sig-name descname">revoke_namespace_permission</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SecurityOperations" title="pysharkbite.SecurityOperations">pysharkbite.SecurityOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg2</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.NamespacePermissions" title="pysharkbite.NamespacePermissions">pysharkbite.NamespacePermissions</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.SecurityOperations.revoke_namespace_permission" title="Permalink to this definition">¶</a></dt>
  <dd><p>Revokes a namespace permission</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SecurityOperations.revoke_system_permission">
  <code class="sig-name descname">revoke_system_permission</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SecurityOperations" title="pysharkbite.SecurityOperations">pysharkbite.SecurityOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SystemPermissions" title="pysharkbite.SystemPermissions">pysharkbite.SystemPermissions</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.SecurityOperations.revoke_system_permission" title="Permalink to this definition">¶</a></dt>
  <dd><p>Revokes a system permission</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SecurityOperations.revoke_table_permission">
  <code class="sig-name descname">revoke_table_permission</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SecurityOperations" title="pysharkbite.SecurityOperations">pysharkbite.SecurityOperations</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg1</span><span class="p">:</span> <span class="n">str</span></em>, <em class="sig-param"><span class="n">arg2</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TablePermissions" title="pysharkbite.TablePermissions">pysharkbite.TablePermissions</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.SecurityOperations.revoke_table_permission" title="Permalink to this definition">¶</a></dt>
  <dd><p>Revokes a table permission</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.Seekable">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">Seekable</code><a class="headerlink" href="#pysharkbite.Seekable" title="Permalink to this definition">¶</a></dt>
  <dd><dl class="py method">
  <dt id="pysharkbite.Seekable.getColumnFamilies">
  <code class="sig-name descname">getColumnFamilies</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Seekable" title="pysharkbite.Seekable">pysharkbite.Seekable</a></span></em><span class="sig-paren">)</span> &#x2192; List<span class="p">[</span>str<span class="p">]</span><a class="headerlink" href="#pysharkbite.Seekable.getColumnFamilies" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the column families for this seekable</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Seekable.getRange">
  <code class="sig-name descname">getRange</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Seekable" title="pysharkbite.Seekable">pysharkbite.Seekable</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.Range" title="pysharkbite.Range">pysharkbite.Range</a><a class="headerlink" href="#pysharkbite.Seekable.getRange" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets this seekable range</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Seekable.isInclusive">
  <code class="sig-name descname">isInclusive</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Seekable" title="pysharkbite.Seekable">pysharkbite.Seekable</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.Seekable.isInclusive" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns true if the column families are inclusive.</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.SequentialRFile">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">SequentialRFile</code><a class="headerlink" href="#pysharkbite.SequentialRFile" title="Permalink to this definition">¶</a></dt>
  <dd><p>Specializd RFile, which is an internal data structure for storing data within Accumulo, to be used when much of the data is similar</p>
  <dl class="py method">
  <dt id="pysharkbite.SequentialRFile.addLocalityGroup">
  <code class="sig-name descname">addLocalityGroup</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SequentialRFile" title="pysharkbite.SequentialRFile">pysharkbite.SequentialRFile</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.SequentialRFile.addLocalityGroup" title="Permalink to this definition">¶</a></dt>
  <dd><p>Adds a locality group to an RFile when writing</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SequentialRFile.append">
  <code class="sig-name descname">append</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SequentialRFile" title="pysharkbite.SequentialRFile">pysharkbite.SequentialRFile</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.KeyValue" title="pysharkbite.KeyValue">pysharkbite.KeyValue</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.SequentialRFile.append" title="Permalink to this definition">¶</a></dt>
  <dd><p>Appends a key and value pair to the RFile</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SequentialRFile.close">
  <code class="sig-name descname">close</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SequentialRFile" title="pysharkbite.SequentialRFile">pysharkbite.SequentialRFile</a></span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.SequentialRFile.close" title="Permalink to this definition">¶</a></dt>
  <dd><p>Closes the RFile</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SequentialRFile.getTop">
  <code class="sig-name descname">getTop</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SequentialRFile" title="pysharkbite.SequentialRFile">pysharkbite.SequentialRFile</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.KeyValue" title="pysharkbite.KeyValue">pysharkbite.KeyValue</a><a class="headerlink" href="#pysharkbite.SequentialRFile.getTop" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns the top key/value</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SequentialRFile.hasNext">
  <code class="sig-name descname">hasNext</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SequentialRFile" title="pysharkbite.SequentialRFile">pysharkbite.SequentialRFile</a></span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.SequentialRFile.hasNext" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns true of further keys exist, false otherwise</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SequentialRFile.next">
  <code class="sig-name descname">next</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SequentialRFile" title="pysharkbite.SequentialRFile">pysharkbite.SequentialRFile</a></span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.SequentialRFile.next" title="Permalink to this definition">¶</a></dt>
  <dd><p>Queues the next key/value pair</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SequentialRFile.seek">
  <code class="sig-name descname">seek</code><span class="sig-paren">(</span><em class="sig-param">self: pysharkbite.SequentialRFile</em>, <em class="sig-param">arg0: cclient::data::streams::StreamRelocation</em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.SequentialRFile.seek" title="Permalink to this definition">¶</a></dt>
  <dd><p>Seeks to the next key and value within the RFile</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.StreamRelocation">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">StreamRelocation</code><a class="headerlink" href="#pysharkbite.StreamRelocation" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.SystemPermissions">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">SystemPermissions</code><a class="headerlink" href="#pysharkbite.SystemPermissions" title="Permalink to this definition">¶</a></dt>
  <dd><p>Members:</p>
  <p>GRANT : Enables grant permission on the system</p>
  <p>CREATE_TABLE : Enables create table permissions on the system</p>
  <p>DROP_TABLE : Enables drop table permissions on the system</p>
  <p>ALTER_TABLE : Enables alter table permissions on the system</p>
  <p>CREATE_USER : Enables create user permissions on the system</p>
  <p>ALTER_USER : Enables alter user permissions on the system</p>
  <p>SYSTEM : Enables system permissions for the user</p>
  <p>CREATE_NAMESPACE : Enables create namespace permissions for the user</p>
  <p>DROP_NAMESPACE : Enables drop namespace permissions for the user</p>
  <p>ALTER_NAMESPACE : Enables the alter namespace permissions for the user</p>
  <dl class="py attribute">
  <dt id="pysharkbite.SystemPermissions.ALTER_NAMESPACE">
  <code class="sig-name descname">ALTER_NAMESPACE</code><em class="property"> = SystemPermissions.ALTER_NAMESPACE</em><a class="headerlink" href="#pysharkbite.SystemPermissions.ALTER_NAMESPACE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.SystemPermissions.ALTER_TABLE">
  <code class="sig-name descname">ALTER_TABLE</code><em class="property"> = SystemPermissions.ALTER_TABLE</em><a class="headerlink" href="#pysharkbite.SystemPermissions.ALTER_TABLE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.SystemPermissions.ALTER_USER">
  <code class="sig-name descname">ALTER_USER</code><em class="property"> = SystemPermissions.ALTER_USER</em><a class="headerlink" href="#pysharkbite.SystemPermissions.ALTER_USER" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.SystemPermissions.CREATE_NAMESPACE">
  <code class="sig-name descname">CREATE_NAMESPACE</code><em class="property"> = SystemPermissions.CREATE_NAMESPACE</em><a class="headerlink" href="#pysharkbite.SystemPermissions.CREATE_NAMESPACE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.SystemPermissions.CREATE_TABLE">
  <code class="sig-name descname">CREATE_TABLE</code><em class="property"> = SystemPermissions.CREATE_TABLE</em><a class="headerlink" href="#pysharkbite.SystemPermissions.CREATE_TABLE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.SystemPermissions.CREATE_USER">
  <code class="sig-name descname">CREATE_USER</code><em class="property"> = SystemPermissions.CREATE_USER</em><a class="headerlink" href="#pysharkbite.SystemPermissions.CREATE_USER" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.SystemPermissions.DROP_NAMESPACE">
  <code class="sig-name descname">DROP_NAMESPACE</code><em class="property"> = SystemPermissions.DROP_NAMESPACE</em><a class="headerlink" href="#pysharkbite.SystemPermissions.DROP_NAMESPACE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.SystemPermissions.DROP_TABLE">
  <code class="sig-name descname">DROP_TABLE</code><em class="property"> = SystemPermissions.DROP_TABLE</em><a class="headerlink" href="#pysharkbite.SystemPermissions.DROP_TABLE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.SystemPermissions.GRANT">
  <code class="sig-name descname">GRANT</code><em class="property"> = SystemPermissions.GRANT</em><a class="headerlink" href="#pysharkbite.SystemPermissions.GRANT" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.SystemPermissions.SYSTEM">
  <code class="sig-name descname">SYSTEM</code><em class="property"> = SystemPermissions.SYSTEM</em><a class="headerlink" href="#pysharkbite.SystemPermissions.SYSTEM" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.SystemPermissions.__entries">
  <code class="sig-name descname">__entries</code><em class="property"> = {'ALTER_NAMESPACE': (SystemPermissions.ALTER_NAMESPACE, 'Enables the alter namespace permissions for the user'), 'ALTER_TABLE': (SystemPermissions.ALTER_TABLE, 'Enables alter table permissions on the system'), 'ALTER_USER': (SystemPermissions.ALTER_USER, 'Enables alter user permissions on the system'), 'CREATE_NAMESPACE': (SystemPermissions.CREATE_NAMESPACE, 'Enables create namespace permissions for the user'), 'CREATE_TABLE': (SystemPermissions.CREATE_TABLE, 'Enables create table permissions on the system'), 'CREATE_USER': (SystemPermissions.CREATE_USER, 'Enables create user permissions on the system'), 'DROP_NAMESPACE': (SystemPermissions.DROP_NAMESPACE, 'Enables drop namespace permissions for the user'), 'DROP_TABLE': (SystemPermissions.DROP_TABLE, 'Enables drop table permissions on the system'), 'GRANT': (SystemPermissions.GRANT, 'Enables grant permission on the system'), 'SYSTEM': (SystemPermissions.SYSTEM, 'Enables system permissions for the user')}</em><a class="headerlink" href="#pysharkbite.SystemPermissions.__entries" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.SystemPermissions.name">
  <em class="property">property </em><code class="sig-name descname">name</code><a class="headerlink" href="#pysharkbite.SystemPermissions.name" title="Permalink to this definition">¶</a></dt>
  <dd><p>(self: handle) -&gt; str</p>
  </dd></dl>

  </dd></dl>

  <dl class="py exception">
  <dt id="pysharkbite.TApplicationException">
  <em class="property">exception </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">TApplicationException</code><a class="headerlink" href="#pysharkbite.TApplicationException" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.TableCompactions">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">TableCompactions</code><a class="headerlink" href="#pysharkbite.TableCompactions" title="Permalink to this definition">¶</a></dt>
  <dd><dl class="py method">
  <dt id="pysharkbite.TableCompactions.getMajorCompactions">
  <code class="sig-name descname">getMajorCompactions</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TableCompactions" title="pysharkbite.TableCompactions">pysharkbite.TableCompactions</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.Compacting" title="pysharkbite.Compacting">pysharkbite.Compacting</a><a class="headerlink" href="#pysharkbite.TableCompactions.getMajorCompactions" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets major compaction stats.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableCompactions.getMinorCompactions">
  <code class="sig-name descname">getMinorCompactions</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TableCompactions" title="pysharkbite.TableCompactions">pysharkbite.TableCompactions</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.Compacting" title="pysharkbite.Compacting">pysharkbite.Compacting</a><a class="headerlink" href="#pysharkbite.TableCompactions.getMinorCompactions" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets minor compaction stats.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableCompactions.getScans">
  <code class="sig-name descname">getScans</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TableCompactions" title="pysharkbite.TableCompactions">pysharkbite.TableCompactions</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.Compacting" title="pysharkbite.Compacting">pysharkbite.Compacting</a><a class="headerlink" href="#pysharkbite.TableCompactions.getScans" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets scan stats.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableCompactions.majors">
  <em class="property">property </em><code class="sig-name descname">majors</code><a class="headerlink" href="#pysharkbite.TableCompactions.majors" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableCompactions.minors">
  <em class="property">property </em><code class="sig-name descname">minors</code><a class="headerlink" href="#pysharkbite.TableCompactions.minors" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableCompactions.scans">
  <em class="property">property </em><code class="sig-name descname">scans</code><a class="headerlink" href="#pysharkbite.TableCompactions.scans" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.TableInfo">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">TableInfo</code><a class="headerlink" href="#pysharkbite.TableInfo" title="Permalink to this definition">¶</a></dt>
  <dd><dl class="py method">
  <dt id="pysharkbite.TableInfo.compaction_info">
  <em class="property">property </em><code class="sig-name descname">compaction_info</code><a class="headerlink" href="#pysharkbite.TableInfo.compaction_info" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableInfo.getCompactioninfo">
  <code class="sig-name descname">getCompactioninfo</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TableInfo" title="pysharkbite.TableInfo">pysharkbite.TableInfo</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.TableCompactions" title="pysharkbite.TableCompactions">pysharkbite.TableCompactions</a><a class="headerlink" href="#pysharkbite.TableInfo.getCompactioninfo" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets compaction info for the table</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableInfo.getOnlineTablets">
  <code class="sig-name descname">getOnlineTablets</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TableInfo" title="pysharkbite.TableInfo">pysharkbite.TableInfo</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.TableInfo.getOnlineTablets" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets online tablets in the table</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableInfo.getRecords">
  <code class="sig-name descname">getRecords</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TableInfo" title="pysharkbite.TableInfo">pysharkbite.TableInfo</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.TableInfo.getRecords" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets records in the table</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableInfo.getRecordsInMemory">
  <code class="sig-name descname">getRecordsInMemory</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TableInfo" title="pysharkbite.TableInfo">pysharkbite.TableInfo</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.TableInfo.getRecordsInMemory" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets records in memory for a table</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableInfo.getTableRates">
  <code class="sig-name descname">getTableRates</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TableInfo" title="pysharkbite.TableInfo">pysharkbite.TableInfo</a></span></em><span class="sig-paren">)</span> &#x2192; <a class="reference internal" href="#pysharkbite.TableRates" title="pysharkbite.TableRates">pysharkbite.TableRates</a><a class="headerlink" href="#pysharkbite.TableInfo.getTableRates" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets table rates for the table</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableInfo.getTablets">
  <code class="sig-name descname">getTablets</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TableInfo" title="pysharkbite.TableInfo">pysharkbite.TableInfo</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.TableInfo.getTablets" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets tablets in the table</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableInfo.online_tables">
  <em class="property">property </em><code class="sig-name descname">online_tables</code><a class="headerlink" href="#pysharkbite.TableInfo.online_tables" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableInfo.records">
  <em class="property">property </em><code class="sig-name descname">records</code><a class="headerlink" href="#pysharkbite.TableInfo.records" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableInfo.records_in_memory">
  <em class="property">property </em><code class="sig-name descname">records_in_memory</code><a class="headerlink" href="#pysharkbite.TableInfo.records_in_memory" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableInfo.table_rates">
  <em class="property">property </em><code class="sig-name descname">table_rates</code><a class="headerlink" href="#pysharkbite.TableInfo.table_rates" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableInfo.tablets">
  <em class="property">property </em><code class="sig-name descname">tablets</code><a class="headerlink" href="#pysharkbite.TableInfo.tablets" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.TablePermissions">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">TablePermissions</code><a class="headerlink" href="#pysharkbite.TablePermissions" title="Permalink to this definition">¶</a></dt>
  <dd><p>Members:</p>
  <p>READ : Enables read permission on the table</p>
  <p>WRITE : Enables write permission on the table</p>
  <p>GRANT : Enables grant permission on the table</p>
  <p>ALTER_TABLE : Enables alter table permissions on the table</p>
  <p>DROP_TABLE : Enables drop table permissions on the table</p>
  <p>BULK_IMPORT : Enables bulk import permissions on the table</p>
  <dl class="py attribute">
  <dt id="pysharkbite.TablePermissions.ALTER_TABLE">
  <code class="sig-name descname">ALTER_TABLE</code><em class="property"> = TablePermissions.ALTER_TABLE</em><a class="headerlink" href="#pysharkbite.TablePermissions.ALTER_TABLE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.TablePermissions.BULK_IMPORT">
  <code class="sig-name descname">BULK_IMPORT</code><em class="property"> = TablePermissions.BULK_IMPORT</em><a class="headerlink" href="#pysharkbite.TablePermissions.BULK_IMPORT" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.TablePermissions.DROP_TABLE">
  <code class="sig-name descname">DROP_TABLE</code><em class="property"> = TablePermissions.DROP_TABLE</em><a class="headerlink" href="#pysharkbite.TablePermissions.DROP_TABLE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.TablePermissions.GRANT">
  <code class="sig-name descname">GRANT</code><em class="property"> = TablePermissions.GRANT</em><a class="headerlink" href="#pysharkbite.TablePermissions.GRANT" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.TablePermissions.READ">
  <code class="sig-name descname">READ</code><em class="property"> = TablePermissions.READ</em><a class="headerlink" href="#pysharkbite.TablePermissions.READ" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.TablePermissions.WRITE">
  <code class="sig-name descname">WRITE</code><em class="property"> = TablePermissions.WRITE</em><a class="headerlink" href="#pysharkbite.TablePermissions.WRITE" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py attribute">
  <dt id="pysharkbite.TablePermissions.__entries">
  <code class="sig-name descname">__entries</code><em class="property"> = {'ALTER_TABLE': (TablePermissions.ALTER_TABLE, 'Enables alter table permissions on the table'), 'BULK_IMPORT': (TablePermissions.BULK_IMPORT, 'Enables bulk import permissions on the table'), 'DROP_TABLE': (TablePermissions.DROP_TABLE, 'Enables drop table permissions on the table'), 'GRANT': (TablePermissions.GRANT, 'Enables grant permission on the table'), 'READ': (TablePermissions.READ, 'Enables read permission on the table'), 'WRITE': (TablePermissions.WRITE, 'Enables write permission on the table')}</em><a class="headerlink" href="#pysharkbite.TablePermissions.__entries" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TablePermissions.name">
  <em class="property">property </em><code class="sig-name descname">name</code><a class="headerlink" href="#pysharkbite.TablePermissions.name" title="Permalink to this definition">¶</a></dt>
  <dd><p>(self: handle) -&gt; str</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.TableRates">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">TableRates</code><a class="headerlink" href="#pysharkbite.TableRates" title="Permalink to this definition">¶</a></dt>
  <dd><dl class="py method">
  <dt id="pysharkbite.TableRates.getIngestRate">
  <code class="sig-name descname">getIngestRate</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TableRates" title="pysharkbite.TableRates">pysharkbite.TableRates</a></span></em><span class="sig-paren">)</span> &#x2192; float<a class="headerlink" href="#pysharkbite.TableRates.getIngestRate" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the ingest rate in records.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableRates.getIngestRateByte">
  <code class="sig-name descname">getIngestRateByte</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TableRates" title="pysharkbite.TableRates">pysharkbite.TableRates</a></span></em><span class="sig-paren">)</span> &#x2192; float<a class="headerlink" href="#pysharkbite.TableRates.getIngestRateByte" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the ingest rate in bytes.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableRates.getQueryRate">
  <code class="sig-name descname">getQueryRate</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TableRates" title="pysharkbite.TableRates">pysharkbite.TableRates</a></span></em><span class="sig-paren">)</span> &#x2192; float<a class="headerlink" href="#pysharkbite.TableRates.getQueryRate" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the query rate in records.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableRates.getQueryRateByte">
  <code class="sig-name descname">getQueryRateByte</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TableRates" title="pysharkbite.TableRates">pysharkbite.TableRates</a></span></em><span class="sig-paren">)</span> &#x2192; float<a class="headerlink" href="#pysharkbite.TableRates.getQueryRateByte" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the query rate in bytes.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableRates.getScanRate">
  <code class="sig-name descname">getScanRate</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TableRates" title="pysharkbite.TableRates">pysharkbite.TableRates</a></span></em><span class="sig-paren">)</span> &#x2192; float<a class="headerlink" href="#pysharkbite.TableRates.getScanRate" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the scan rate.</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableRates.query_rate_byte">
  <em class="property">property </em><code class="sig-name descname">query_rate_byte</code><a class="headerlink" href="#pysharkbite.TableRates.query_rate_byte" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TableRates.scan_rate">
  <em class="property">property </em><code class="sig-name descname">scan_rate</code><a class="headerlink" href="#pysharkbite.TableRates.scan_rate" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.TabletServerStatus">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">TabletServerStatus</code><a class="headerlink" href="#pysharkbite.TabletServerStatus" title="Permalink to this definition">¶</a></dt>
  <dd><dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.data_cache_hits">
  <em class="property">property </em><code class="sig-name descname">data_cache_hits</code><a class="headerlink" href="#pysharkbite.TabletServerStatus.data_cache_hits" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.data_cache_requests">
  <em class="property">property </em><code class="sig-name descname">data_cache_requests</code><a class="headerlink" href="#pysharkbite.TabletServerStatus.data_cache_requests" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.flushes">
  <em class="property">property </em><code class="sig-name descname">flushes</code><a class="headerlink" href="#pysharkbite.TabletServerStatus.flushes" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.getDataCacheHits">
  <code class="sig-name descname">getDataCacheHits</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TabletServerStatus" title="pysharkbite.TabletServerStatus">pysharkbite.TabletServerStatus</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.TabletServerStatus.getDataCacheHits" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets data cache hits against the server</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.getDataCacheRequests">
  <code class="sig-name descname">getDataCacheRequests</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TabletServerStatus" title="pysharkbite.TabletServerStatus">pysharkbite.TabletServerStatus</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.TabletServerStatus.getDataCacheRequests" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets data cache requests against the server</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.getFlushes">
  <code class="sig-name descname">getFlushes</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TabletServerStatus" title="pysharkbite.TabletServerStatus">pysharkbite.TabletServerStatus</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.TabletServerStatus.getFlushes" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the number of flushes on the server</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.getHoldTime">
  <code class="sig-name descname">getHoldTime</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TabletServerStatus" title="pysharkbite.TabletServerStatus">pysharkbite.TabletServerStatus</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.TabletServerStatus.getHoldTime" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the hold time</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.getIndexCacheHits">
  <code class="sig-name descname">getIndexCacheHits</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TabletServerStatus" title="pysharkbite.TabletServerStatus">pysharkbite.TabletServerStatus</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.TabletServerStatus.getIndexCacheHits" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets index cache hits against the server</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.getLastContact">
  <code class="sig-name descname">getLastContact</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TabletServerStatus" title="pysharkbite.TabletServerStatus">pysharkbite.TabletServerStatus</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.TabletServerStatus.getLastContact" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the last contact time of the server</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.getLogSorts">
  <code class="sig-name descname">getLogSorts</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TabletServerStatus" title="pysharkbite.TabletServerStatus">pysharkbite.TabletServerStatus</a></span></em><span class="sig-paren">)</span> &#x2192; List<span class="p">[</span><a class="reference internal" href="#pysharkbite.RecoveryStatus" title="pysharkbite.RecoveryStatus">pysharkbite.RecoveryStatus</a><span class="p">]</span><a class="headerlink" href="#pysharkbite.TabletServerStatus.getLogSorts" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the number of log sorts</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.getLookups">
  <code class="sig-name descname">getLookups</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TabletServerStatus" title="pysharkbite.TabletServerStatus">pysharkbite.TabletServerStatus</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.TabletServerStatus.getLookups" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets lookups against the server</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.getName">
  <code class="sig-name descname">getName</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TabletServerStatus" title="pysharkbite.TabletServerStatus">pysharkbite.TabletServerStatus</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.TabletServerStatus.getName" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the name of the server</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.getOsLoad">
  <code class="sig-name descname">getOsLoad</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TabletServerStatus" title="pysharkbite.TabletServerStatus">pysharkbite.TabletServerStatus</a></span></em><span class="sig-paren">)</span> &#x2192; float<a class="headerlink" href="#pysharkbite.TabletServerStatus.getOsLoad" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the load of the server</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.getSyncs">
  <code class="sig-name descname">getSyncs</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TabletServerStatus" title="pysharkbite.TabletServerStatus">pysharkbite.TabletServerStatus</a></span></em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.TabletServerStatus.getSyncs" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the number of syncs on the server</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.getTableMap">
  <code class="sig-name descname">getTableMap</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.TabletServerStatus" title="pysharkbite.TabletServerStatus">pysharkbite.TabletServerStatus</a></span></em><span class="sig-paren">)</span> &#x2192; Dict<span class="p">[</span>str<span class="p">, </span><a class="reference internal" href="#pysharkbite.TableInfo" title="pysharkbite.TableInfo">pysharkbite.TableInfo</a><span class="p">]</span><a class="headerlink" href="#pysharkbite.TabletServerStatus.getTableMap" title="Permalink to this definition">¶</a></dt>
  <dd><p>Gets the table map</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.hold_time">
  <em class="property">property </em><code class="sig-name descname">hold_time</code><a class="headerlink" href="#pysharkbite.TabletServerStatus.hold_time" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.index_cache_hits">
  <em class="property">property </em><code class="sig-name descname">index_cache_hits</code><a class="headerlink" href="#pysharkbite.TabletServerStatus.index_cache_hits" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.last_contact">
  <em class="property">property </em><code class="sig-name descname">last_contact</code><a class="headerlink" href="#pysharkbite.TabletServerStatus.last_contact" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.log_sorts">
  <em class="property">property </em><code class="sig-name descname">log_sorts</code><a class="headerlink" href="#pysharkbite.TabletServerStatus.log_sorts" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.lookups">
  <em class="property">property </em><code class="sig-name descname">lookups</code><a class="headerlink" href="#pysharkbite.TabletServerStatus.lookups" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.name">
  <em class="property">property </em><code class="sig-name descname">name</code><a class="headerlink" href="#pysharkbite.TabletServerStatus.name" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.os_load">
  <em class="property">property </em><code class="sig-name descname">os_load</code><a class="headerlink" href="#pysharkbite.TabletServerStatus.os_load" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.syncs">
  <em class="property">property </em><code class="sig-name descname">syncs</code><a class="headerlink" href="#pysharkbite.TabletServerStatus.syncs" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.TabletServerStatus.table_map">
  <em class="property">property </em><code class="sig-name descname">table_map</code><a class="headerlink" href="#pysharkbite.TabletServerStatus.table_map" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.Value">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">Value</code><a class="headerlink" href="#pysharkbite.Value" title="Permalink to this definition">¶</a></dt>
  <dd><p>Accumulo value</p>
  <dl class="py method">
  <dt id="pysharkbite.Value.get">
  <code class="sig-name descname">get</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Value" title="pysharkbite.Value">pysharkbite.Value</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.Value.get" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns the value as a UTF-8 string</p>
  </dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.Value.get_bytes">
  <code class="sig-name descname">get_bytes</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.Value" title="pysharkbite.Value">pysharkbite.Value</a></span></em><span class="sig-paren">)</span> &#x2192; bytes<a class="headerlink" href="#pysharkbite.Value.get_bytes" title="Permalink to this definition">¶</a></dt>
  <dd><p>Returns the bytes as python bytes</p>
  </dd></dl>

  </dd></dl>

  <dl class="py class">
  <dt id="pysharkbite.ZookeeperInstance">
  <em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">ZookeeperInstance</code><a class="headerlink" href="#pysharkbite.ZookeeperInstance" title="Permalink to this definition">¶</a></dt>
  <dd><p>Zookeeper instance enables connectivity to a zookeper quorum</p>
  <dl class="py method">
  <dt id="pysharkbite.ZookeeperInstance.getInstanceId">
  <code class="sig-name descname">getInstanceId</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.ZookeeperInstance" title="pysharkbite.ZookeeperInstance">pysharkbite.ZookeeperInstance</a></span></em>, <em class="sig-param"><span class="n">retry</span><span class="p">:</span> <span class="n">bool</span> <span class="o">=</span> <span class="default_value">False</span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.ZookeeperInstance.getInstanceId" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  <dl class="py method">
  <dt id="pysharkbite.ZookeeperInstance.getInstanceName">
  <code class="sig-name descname">getInstanceName</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.ZookeeperInstance" title="pysharkbite.ZookeeperInstance">pysharkbite.ZookeeperInstance</a></span></em><span class="sig-paren">)</span> &#x2192; str<a class="headerlink" href="#pysharkbite.ZookeeperInstance.getInstanceName" title="Permalink to this definition">¶</a></dt>
  <dd></dd></dl>

  </dd></dl>

    
  </embed>