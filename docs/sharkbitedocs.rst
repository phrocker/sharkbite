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
	<dt id="pysharkbite.AccumuloConnector.securityOps">
	<code class="sig-name descname">securityOps</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloConnector" title="pysharkbite.AccumuloConnector">pysharkbite.AccumuloConnector</a></span></em><span class="sig-paren">)</span> &#x2192; interconnect::SecurityOperations<a class="headerlink" href="#pysharkbite.AccumuloConnector.securityOps" title="Permalink to this definition">¶</a></dt>
	<dd><p>Return the security operations object</p>
	</dd></dl>
	
	<dl class="py method">
	<dt id="pysharkbite.AccumuloConnector.tableOps">
	<code class="sig-name descname">tableOps</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.AccumuloConnector" title="pysharkbite.AccumuloConnector">pysharkbite.AccumuloConnector</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">str</span></em><span class="sig-paren">)</span> &#x2192; interconnect::AccumuloTableOperations<a class="headerlink" href="#pysharkbite.AccumuloConnector.tableOps" title="Permalink to this definition">¶</a></dt>
	<dd><p>Return the table operations object</p>
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
	<dd><p>Create scanner</p>
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
	<code class="sig-name descname">addRange</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.BatchScanner" title="pysharkbite.BatchScanner">pysharkbite.BatchScanner</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">pysharkbite.Range</span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.BatchScanner.addRange" title="Permalink to this definition">¶</a></dt>
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
	<code class="sig-name descname">getResultSet</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.BatchScanner" title="pysharkbite.BatchScanner">pysharkbite.BatchScanner</a></span></em><span class="sig-paren">)</span> &#x2192; pysharkbite.Results<a class="headerlink" href="#pysharkbite.BatchScanner.getResultSet" title="Permalink to this definition">¶</a></dt>
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
	
	</dd></dl>
	
	<dl class="py class">
	<dt id="pysharkbite.BatchWriter">
	<em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">BatchWriter</code><a class="headerlink" href="#pysharkbite.BatchWriter" title="Permalink to this definition">¶</a></dt>
	<dd><p>Batch writer to be constructed, from TableOperations</p>
	<dl class="py method">
	<dt id="pysharkbite.BatchWriter.addMutation">
	<code class="sig-name descname">addMutation</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.BatchWriter" title="pysharkbite.BatchWriter">pysharkbite.BatchWriter</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">pysharkbite.Mutation</span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.BatchWriter.addMutation" title="Permalink to this definition">¶</a></dt>
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
	<dt id="pysharkbite.Hdfs">
	<em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">Hdfs</code><a class="headerlink" href="#pysharkbite.Hdfs" title="Permalink to this definition">¶</a></dt>
	<dd><p>HDFS refernce object</p>
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
	<code class="sig-name descname">getTop</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.RFile" title="pysharkbite.RFile">pysharkbite.RFile</a></span></em><span class="sig-paren">)</span> &#x2192; pysharkbite.KeyValue<a class="headerlink" href="#pysharkbite.RFile.getTop" title="Permalink to this definition">¶</a></dt>
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
	<dt id="pysharkbite.ScannerOptions">
	<em class="property">class </em><code class="sig-prename descclassname">pysharkbite.</code><code class="sig-name descname">ScannerOptions</code><a class="headerlink" href="#pysharkbite.ScannerOptions" title="Permalink to this definition">¶</a></dt>
	<dd><p>Members:</p>
	<p>HedgedReads : Enables hedged reads</p>
	<p>RFileScanOnly : Enables the RFile Scanner</p>
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
	<dt id="pysharkbite.SecurityOperations.grantAuthorizations">
	<code class="sig-name descname">grantAuthorizations</code><span class="sig-paren">(</span><em class="sig-param">self: pysharkbite.SecurityOperations</em>, <em class="sig-param">arg0: cclient::data::security::Authorizations</em>, <em class="sig-param">arg1: str</em><span class="sig-paren">)</span> &#x2192; int<a class="headerlink" href="#pysharkbite.SecurityOperations.grantAuthorizations" title="Permalink to this definition">¶</a></dt>
	<dd><p>Get user authorizations</p>
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
	<code class="sig-name descname">append</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SequentialRFile" title="pysharkbite.SequentialRFile">pysharkbite.SequentialRFile</a></span></em>, <em class="sig-param"><span class="n">arg0</span><span class="p">:</span> <span class="n">pysharkbite.KeyValue</span></em><span class="sig-paren">)</span> &#x2192; bool<a class="headerlink" href="#pysharkbite.SequentialRFile.append" title="Permalink to this definition">¶</a></dt>
	<dd><p>Appends a key and value pair to the RFile</p>
	</dd></dl>
	
	<dl class="py method">
	<dt id="pysharkbite.SequentialRFile.close">
	<code class="sig-name descname">close</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SequentialRFile" title="pysharkbite.SequentialRFile">pysharkbite.SequentialRFile</a></span></em><span class="sig-paren">)</span> &#x2192; None<a class="headerlink" href="#pysharkbite.SequentialRFile.close" title="Permalink to this definition">¶</a></dt>
	<dd><p>Closes the RFile</p>
	</dd></dl>
	
	<dl class="py method">
	<dt id="pysharkbite.SequentialRFile.getTop">
	<code class="sig-name descname">getTop</code><span class="sig-paren">(</span><em class="sig-param"><span class="n">self</span><span class="p">:</span> <span class="n"><a class="reference internal" href="#pysharkbite.SequentialRFile" title="pysharkbite.SequentialRFile">pysharkbite.SequentialRFile</a></span></em><span class="sig-paren">)</span> &#x2192; pysharkbite.KeyValue<a class="headerlink" href="#pysharkbite.SequentialRFile.getTop" title="Permalink to this definition">¶</a></dt>
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
	
	  
	</embed>