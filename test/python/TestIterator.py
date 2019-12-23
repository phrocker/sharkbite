from testmodule import *

class TestWrites(TestRunner):
		
	def __init__(self):
		super().__init__()
	
	def mthd(self):
	
		import pysharkbite

		
		tableOperations = super().getTableOperations()
			
		if not tableOperations.exists(False):
		    print ("Creating table")
		    if not tableOperations.create(False):
		    	print("Could not create table")  
		else:
		    print ("Table already exists, so not creating it")  
		
		
		auths = pysharkbite.Authorizations()
		
		""" Add authorizations """ 
		""" mutation.put("cf","cq","cv",1569786960) """
		
		
		writer = tableOperations.createWriter(auths, 10)
		
		mutation = pysharkbite.Mutation("sow2");    
		
		mutation.put("cf","cq","",1569786960, "value")
		mutation.put("cf2","cq","",1569786960, "value")
		""" no value """
		mutation.put("cf3","cq","",1569786960, "value") 
		
		writer.addMutation( mutation )
		
		writer.close()
		
		writer = tableOperations.createWriter(auths, 10)
		
		rng = range(0,1000)
		for i in rng:
			row = ("row%i" % (i+5))
			mutation = pysharkbite.Mutation(row);    
			mutation.put("cf","cq","",1569786960, "value")
			writer.addMutation(mutation)
			
		writer.close()
		
		print("written")
		
		""" auths.addAuthorization("cv") """
		
		scanner = tableOperations.createScanner(auths, 2)
		
		accumuloRange = pysharkbite.Range("sow",True,"sow3",False)
		
		scanner.addRange( accumuloRange )
		
		iterator = pysharkbite.PythonIterator("PythonIterator",100)
		iterator = iterator.onNext("lambda x : sharkbite_iterator.Key( x.getRow(), 'new cf', x.getColumnQualifier()) ")
		scanner.addIterator(iterator)
		
		resultset = scanner.getResultSet()
		
		for keyvalue in resultset:
			key = keyvalue.getKey()
			assert( "sow2" == key.getRow() )
			value = keyvalue.getValue()
			if "cf" == key.getColumnFamily():
				sys.exit(154)
			if "new cf" == key.getColumnFamily():
				assert( ""  == value.get() )
		    
		scanner = tableOperations.createScanner(auths, 2)
		
		accumuloRange = pysharkbite.Range("sow",True,"sow3",False)
		
		scanner.addRange( accumuloRange )
		
		iterator = pysharkbite.PythonIterator("PythonIterator",100)
		iterator = iterator.onNext("lambda x : sharkbite_iterator.Key( x.getRow(), x.getColumnFamily(), 'new cq') ")
		scanner.addIterator(iterator)
		
		resultset = scanner.getResultSet()
		
		for keyvalue in resultset:
			key = keyvalue.getKey()
			assert( "sow2" == key.getRow() )
			value = keyvalue.getValue()
			if "cq" == key.getColumnQualifier():
				sys.exit(154)
				
		""" delete your table if user did not create temp """
		
		tableOperations.remove()


runner = TestWrites()
runner.mthd()