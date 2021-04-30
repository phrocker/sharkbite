from testmodule import *

class TestRFileWrites(TestRunner):
		
	def __init__(self):
		parser = ArgumentParser(description="This is an Apache Accummulo Python connector")

		parser.add_argument("-i", "--instance", dest="instance",
							help="Apache Accumulo Instance Name", required=True)
		parser.add_argument("-z", "--zookeepers", dest="zookeepers",
							help="Comma Separated Zookeeper List", required=True)
		parser.add_argument("-u", "--username", dest="username",
							help="User to access Apache Accumulo", required=True)
		parser.add_argument("-p", "--password", dest="password",
							help="Password to access Apache Accumulo. May also be supplied at the command line")
		parser.add_argument("-t", "--table", dest="table",
							help="Table to create/update")
		parser.add_argument("-s", "--solocation", dest="shardobject",
							help="Shared object Location")
		args = parser.parse_args()

		instance = args.instance
		zookeepers = args.zookeepers
		password = args.password
		table = args.table
		dll = args.shardobject

		print ("Opening ",dll)
		py = cdll.LoadLibrary(dll)
		import sharkbite

	def writerfile(self):

		import sharkbite
		rfile = sharkbite.RFileOperations.openForWrite("blah.rf")

		for x in range(1000):
			key = sharkbite.Key(row=str("row" + str(x)))
			value = sharkbite.Value()
			kv = sharkbite.KeyValue(key,value)
			rfile.append(kv)
		
		rfile.close()

	
	def mthd(self):
		
		import sharkbite
		
		sharkbite.LoggingConfiguration.enableTraceLogger()

		self.writerfile()
		
		readrfile = sharkbite.RFileOperations.sequentialRead("blah.rf")

		accrange = sharkbite.Range()

		seekable = sharkbite.Seekable(accrange)
		readrfile.seek( seekable)
		i = 0
		while readrfile.hasNext():
			assert( str("row" + str(i)) == readrfile.getTopKey().getRow() )		
			i=i+1
			readrfile.next()
		assert(i==1000)




runner = TestRFileWrites()
runner.mthd()
